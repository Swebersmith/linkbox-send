package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type App struct {
	cfg                                              Config
	db                                               *sql.DB
	log                                              *slog.Logger
	files, chunks                                    *os.Root
	proxies                                          []*net.IPNet
	locks                                            [256]sync.RWMutex
	chunkLocks                                       [1024]sync.Mutex
	capacityMu                                       sync.Mutex
	rates                                            limiter
	chunkSlots, hashSlots, mergeSlots, downloadSlots chan struct{}
	ctx                                              context.Context
	cancel                                           context.CancelFunc
	wg                                               sync.WaitGroup
	handler                                          http.Handler
}

func New(cfg Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, p := range []string{cfg.StoragePath, cfg.ChunkPath} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	db, err := openDB(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, db: db, log: logger, chunkSlots: make(chan struct{}, cfg.MaxChunkRequests), hashSlots: make(chan struct{}, 4), mergeSlots: make(chan struct{}, 1), downloadSlots: make(chan struct{}, 64)}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.files, err = os.OpenRoot(cfg.StoragePath)
	if err != nil {
		db.Close()
		return nil, err
	}
	a.chunks, err = os.OpenRoot(cfg.ChunkPath)
	if err != nil {
		a.files.Close()
		db.Close()
		return nil, err
	}
	for _, cidr := range strings.Split(cfg.TrustedProxyCIDRs, ",") {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		_, block, e := net.ParseCIDR(strings.TrimSpace(cidr))
		if e != nil {
			a.Close()
			return nil, e
		}
		a.proxies = append(a.proxies, block)
	}
	if err = a.bootstrap(); err != nil {
		a.Close()
		return nil, err
	}
	// A merging upload retains its chunks until the metadata commit. It can safely be retried after a crash.
	if _, err = db.Exec("UPDATE uploads SET status='uploading',error='Server restarted; resume to finish' WHERE status='merging'"); err != nil {
		a.Close()
		return nil, err
	}
	a.routes()
	return a, nil
}
func (a *App) Close() error {
	a.cancel()
	a.wg.Wait()
	if a.files != nil {
		a.files.Close()
	}
	if a.chunks != nil {
		a.chunks.Close()
	}
	return a.db.Close()
}
func (a *App) Handler() http.Handler { return a.handler }
func (a *App) StartWorkers() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.cleanup()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.cleanup()
			case <-a.ctx.Done():
				return
			}
		}
	}()
}
func stripe(s string, n uint32) uint32         { h := fnv.New32a(); h.Write([]byte(s)); return h.Sum32() % n }
func (a *App) lock(id string) *sync.RWMutex    { return &a.locks[stripe(id, 256)] }
func (a *App) chunkLock(id string) *sync.Mutex { return &a.chunkLocks[stripe(id, 1024)] }

func (a *App) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if e := a.db.PingContext(r.Context()); e != nil {
			problem(w, 503, "database unavailable")
			return
		}
		jsonOut(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("POST /api/auth/login", a.login)
	m.HandleFunc("GET /api/auth/me", a.requireUser(a.me))
	m.HandleFunc("POST /api/auth/logout", a.requireUser(a.logout))
	m.HandleFunc("POST /api/uploads", a.requireUser(a.createUpload))
	m.HandleFunc("GET /api/uploads", a.requireUser(a.listUploads))
	m.HandleFunc("GET /api/uploads/{uploadId}", a.requireUser(a.getUpload))
	m.HandleFunc("PUT /api/uploads/{uploadId}/chunks/{index}", a.requireUser(a.putChunk))
	m.HandleFunc("POST /api/uploads/{uploadId}/complete", a.requireUser(a.completeUpload))
	m.HandleFunc("DELETE /api/uploads/{uploadId}", a.requireUser(a.cancelUpload))
	m.HandleFunc("GET /api/files", a.requireUser(a.listFiles))
	m.HandleFunc("DELETE /api/files/{fileId}", a.requireUser(a.deleteFile))
	m.HandleFunc("PATCH /api/files/{fileId}", a.requireUser(a.setRetention))
	m.HandleFunc("POST /api/files/{fileId}/shares", a.requireUser(a.createShare))
	m.HandleFunc("GET /api/files/{fileId}/shares", a.requireUser(a.listShares))
	m.HandleFunc("DELETE /api/shares/{shareId}", a.requireUser(a.revokeShare))
	m.HandleFunc("GET /api/shares/{token}", a.publicShare)
	m.HandleFunc("POST /api/shares/{token}/verify", a.verifyShare)
	m.HandleFunc("GET /api/download/{token}", a.download)
	m.HandleFunc("GET /api/dashboard", a.requireUser(a.dashboard))
	m.HandleFunc("GET /api/settings", a.requireUser(a.settings))
	m.HandleFunc("GET /api/users", a.requireUser(a.listUsers))
	m.HandleFunc("POST /api/users", a.requireUser(a.createUser))
	m.HandleFunc("POST /api/auth/password", a.requireUser(a.changePassword))
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "private, no-store")
		defer func() {
			if rec := recover(); rec != nil {
				a.log.Error("request panic", "method", r.Method)
				problem(w, 500, "internal server error")
			}
		}()
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("Origin") != a.cfg.AppURL {
				problem(w, 403, "invalid request origin")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, message string) {
	jsonOut(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		problem(w, 415, "expected application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		problem(w, 400, "invalid JSON body")
		return false
	}
	if e := d.Decode(new(any)); !errors.Is(e, io.EOF) {
		problem(w, 400, "unexpected trailing JSON")
		return false
	}
	return true
}
func (a *App) internal(w http.ResponseWriter, event string, err error) {
	a.log.Error(event, "error", err)
	problem(w, 500, "operation failed; please retry")
}
func unixNow() int64     { return time.Now().Unix() }
func iso(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) }
func nullableTime(t sql.NullInt64) any {
	if !t.Valid {
		return nil
	}
	return iso(t.Int64)
}
func syncDirectory(root *os.Root, path string) error {
	f, e := root.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return syncDir(f)
}
func filePath(id string) string { return filepath.Join(id, "payload") }
