package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	a      *App
	t      *testing.T
	cfg    Config
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{AppURL: "https://send.test", DatabasePath: filepath.Join(dir, "database", "test.db"), StoragePath: filepath.Join(dir, "files"), ChunkPath: filepath.Join(dir, "chunks"), AdminUsername: "admin", AdminPassword: "correct-horse-test-password", SessionSecret: strings.Repeat("s", 64), ChunkSize: 4, MaxFileSize: 1 << 30, ReservedBytes: 0, Concurrency: 4, MaxActiveUploads: 100, MaxUploadsPerUser: 20, MaxChunkRequests: 16, UploadExpiry: 24 * time.Hour, SessionExpiry: 24 * time.Hour, SecureCookies: true}
	a, e := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{a: a, t: t, cfg: cfg}
	t.Cleanup(func() { f.a.Close() })
	r := f.json("POST", "/api/auth/login", map[string]string{"username": cfg.AdminUsername, "password": cfg.AdminPassword})
	f.status(r, 200)
	var body struct {
		CSRF string `json:"csrfToken"`
	}
	json.Unmarshal(r.Body.Bytes(), &body)
	f.csrf = body.CSRF
	for _, c := range r.Result().Cookies() {
		if c.Name == a.sessionName() {
			f.cookie = c
		}
	}
	if f.cookie == nil || !f.cookie.HttpOnly || !f.cookie.Secure || f.cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("insecure session cookie")
	}
	return f
}
func (f *fixture) req(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Origin", f.cfg.AppURL)
	req.Header.Set("X-CSRF-Token", f.csrf)
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	f.a.Handler().ServeHTTP(rr, req)
	return rr
}
func (f *fixture) json(method, path string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	return f.req(method, path, b, map[string]string{"Content-Type": "application/json"})
}
func (f *fixture) status(r *httptest.ResponseRecorder, want int) {
	f.t.Helper()
	if r.Code != want {
		f.t.Fatalf("status=%d want=%d body=%s", r.Code, want, r.Body.String())
	}
}
func (f *fixture) init(name string, size int) Upload {
	r := f.json("POST", "/api/uploads", map[string]any{"name": name, "size": size, "chunkSize": f.cfg.ChunkSize, "mimeType": "application/octet-stream"})
	f.status(r, 201)
	var u Upload
	if e := json.Unmarshal(r.Body.Bytes(), &u); e != nil {
		f.t.Fatal(e)
	}
	return u
}
func (f *fixture) chunk(u Upload, index int, body string) *httptest.ResponseRecorder {
	sum := sha256.Sum256([]byte(body))
	return f.req("PUT", fmt.Sprintf("/api/uploads/%s/chunks/%d", u.ID, index), []byte(body), map[string]string{"Content-Type": "application/octet-stream", "X-Chunk-SHA256": hex.EncodeToString(sum[:])})
}
func (f *fixture) finish(u Upload) Upload {
	r := f.json("POST", "/api/uploads/"+u.ID+"/complete", map[string]any{})
	if r.Code != 200 && r.Code != 202 {
		f.t.Fatalf("complete: %d %s", r.Code, r.Body.String())
	}
	for i := 0; i < 500; i++ {
		r = f.req("GET", "/api/uploads/"+u.ID, nil, nil)
		f.status(r, 200)
		u = Upload{}
		json.Unmarshal(r.Body.Bytes(), &u)
		if u.Status == "complete" {
			return u
		}
		if u.Error != "" {
			f.t.Fatal(u.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("merge timed out")
	return u
}
func (f *fixture) file() Upload {
	u := f.init("测试 résumé.zip", 10)
	for i, p := range []string{"abcd", "efgh", "ij"} {
		f.status(f.chunk(u, i, p), 204)
	}
	return f.finish(u)
}
func (f *fixture) share(u Upload, password string, max int, expires int) string {
	r := f.json("POST", "/api/files/"+u.FileID+"/shares", map[string]any{"password": password, "maxDownloads": max, "expiresIn": expires})
	f.status(r, 201)
	var s struct {
		Token string `json:"token"`
	}
	json.Unmarshal(r.Body.Bytes(), &s)
	if !tokenRE.MatchString(s.Token) {
		f.t.Fatal("weak token")
	}
	return s.Token
}

func TestUploadInit(t *testing.T) {
	f := setup(t)
	u := f.init("archive.zip", 10)
	if u.Total != 3 || u.ChunkSize != 4 || len(u.Uploaded) != 0 || !uuidRE.MatchString(u.ID) {
		t.Fatalf("bad upload: %+v", u)
	}
	var mode string
	f.a.db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatal("WAL disabled")
	}
	f.status(f.json("POST", "/api/uploads", map[string]any{"name": "a", "size": 1, "chunkSize": 8}), 400)
}
func TestChunkUpload(t *testing.T) {
	f := setup(t)
	u := f.init("a.bin", 6)
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.status(f.chunk(u, 0, "efgh"), 409)
	f.status(f.chunk(u, 1, "ef"), 204)
	f.status(f.chunk(u, 2, "xx"), 400)
	f.status(f.chunk(u, 1, "too long"), 400)
	f.status(f.req("PUT", "/api/uploads/"+u.ID+"/chunks/1", []byte("ef"), map[string]string{"Content-Type": "application/octet-stream", "X-Chunk-SHA256": strings.Repeat("0", 64)}), 422)
	data, e := os.ReadFile(filepath.Join(f.cfg.ChunkPath, u.ID, "0.part"))
	if e != nil || string(data) != "abcd" {
		t.Fatal("chunk not stored")
	}
}
func TestResumeAfterRestart(t *testing.T) {
	f := setup(t)
	u := f.init("a.bin", 10)
	f.status(f.chunk(u, 1, "efgh"), 204)
	f.a.Close()
	var e error
	f.a, e = New(f.cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	r := f.req("GET", "/api/uploads/"+u.ID, nil, nil)
	f.status(r, 200)
	json.Unmarshal(r.Body.Bytes(), &u)
	if len(u.Uploaded) != 1 || u.Uploaded[0] != 1 {
		t.Fatal("resume chunks lost")
	}
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.status(f.chunk(u, 2, "ij"), 204)
	f.finish(u)
}
func TestMissingChunks(t *testing.T) {
	f := setup(t)
	u := f.init("a", 10)
	f.status(f.chunk(u, 1, "efgh"), 204)
	r := f.json("POST", "/api/uploads/"+u.ID+"/complete", map[string]any{})
	f.status(r, 409)
	var body struct {
		Missing []int `json:"missingChunks"`
	}
	json.Unmarshal(r.Body.Bytes(), &body)
	if fmt.Sprint(body.Missing) != "[0 2]" {
		t.Fatal(body.Missing)
	}
}
func TestCompleteUpload(t *testing.T) {
	f := setup(t)
	u := f.file()
	f.finish(u)
	b, e := os.ReadFile(filepath.Join(f.cfg.StoragePath, u.FileID, "payload"))
	if e != nil || string(b) != "abcdefghij" {
		t.Fatalf("payload %q %v", b, e)
	}
	var digest string
	f.a.db.QueryRow("SELECT sha256 FROM files WHERE id=?", u.FileID).Scan(&digest)
	sum := sha256.Sum256(b)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong file hash")
	}
	if _, e = os.Stat(filepath.Join(f.cfg.ChunkPath, u.ID)); !os.IsNotExist(e) {
		t.Fatal("chunks not removed")
	}
	empty := f.init("empty", 0)
	f.finish(empty)
}
func TestRangeDownload(t *testing.T) {
	f := setup(t)
	token := f.share(f.file(), "", 0, 0)
	r := f.req("GET", "/api/download/"+token, nil, map[string]string{"Range": "bytes=3-6"})
	f.status(r, 206)
	if r.Body.String() != "defg" || r.Header().Get("Content-Range") != "bytes 3-6/10" || r.Header().Get("Content-Length") != "4" || r.Header().Get("Accept-Ranges") != "bytes" || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Header().Get("Content-Disposition"), "filename*=utf-8''") {
		t.Fatalf("bad range response: %v %s", r.Header(), r.Body.String())
	}
	r = f.req("GET", "/api/download/"+token, nil, map[string]string{"Range": "bytes=7-"})
	f.status(r, 206)
	if r.Body.String() != "hij" {
		t.Fatal(r.Body.String())
	}
	f.status(f.req("GET", "/api/download/"+token, nil, map[string]string{"Range": "bytes=999-"}), 416)
	f.status(f.req("HEAD", "/api/download/"+token, nil, nil), 200)
}
func TestExpiredShare(t *testing.T) {
	f := setup(t)
	token := f.share(f.file(), "", 0, 3600)
	f.a.db.Exec("UPDATE shares SET expires_at=? WHERE token=?", unixNow()-1, token)
	f.status(f.req("GET", "/api/download/"+token, nil, nil), 410)
	if e := f.a.cleanExpired(); e != nil {
		t.Fatal(e)
	}
	f.status(f.req("GET", "/api/shares/"+token, nil, nil), 410)
}
func TestPasswordShare(t *testing.T) {
	f := setup(t)
	token := f.share(f.file(), "share-passphrase-123", 0, 86400)
	f.status(f.req("GET", "/api/download/"+token, nil, nil), 401)
	r := f.req("GET", "/api/shares/"+token, nil, nil)
	if strings.Contains(r.Body.String(), "fileName") {
		t.Fatal("password protected metadata leaked")
	}
	f.status(f.json("POST", "/api/shares/"+token+"/verify", map[string]string{"password": "incorrect"}), 401)
	r = f.json("POST", "/api/shares/"+token+"/verify", map[string]string{"password": "share-passphrase-123"})
	f.status(r, 200)
	cookies := r.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatal("missing grant cookie")
	}
	f.status(f.req("GET", "/api/download/"+token, nil, map[string]string{"Cookie": cookies[0].String()}), 200)
	var hash string
	f.a.db.QueryRow("SELECT password_hash FROM shares WHERE token=?", token).Scan(&hash)
	if hash == "share-passphrase-123" || !strings.HasPrefix(hash, "$2") {
		t.Fatal("share password not hashed")
	}
}
func TestMaxDownloadsConcurrent(t *testing.T) {
	f := setup(t)
	token := f.share(f.file(), "", 1, 0)
	f.status(f.req("HEAD", "/api/download/"+token, nil, nil), 200)
	f.status(f.req("GET", "/api/download/"+token, nil, map[string]string{"Range": "bytes=999-"}), 416)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- f.req("GET", "/api/download/"+token, nil, nil).Code }()
	}
	wg.Wait()
	close(codes)
	success, gone := 0, 0
	for c := range codes {
		if c == 200 {
			success++
		} else if c == 410 {
			gone++
		} else {
			t.Fatalf("unexpected %d", c)
		}
	}
	if success != 1 || gone != 7 {
		t.Fatalf("counter race: %d %d", success, gone)
	}
}
func TestPathTraversal(t *testing.T) {
	f := setup(t)
	for _, name := range []string{"../secret", "..\\secret", "/etc/passwd", "C:\\payload", "a\r\nHeader: bad", "..", "\x00"} {
		f.status(f.json("POST", "/api/uploads", map[string]any{"name": name, "size": 1}), 400)
	}
	f.status(f.req("GET", "/api/uploads/not-a-uuid", nil, nil), 400)
	u := f.init("' OR 1=1; <svg onload=alert(1)>.zip", 0)
	_ = u
}
func TestCancelAndDelete(t *testing.T) {
	f := setup(t)
	u := f.init("cancel", 5)
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.status(f.req("DELETE", "/api/uploads/"+u.ID, nil, nil), 204)
	f.status(f.req("GET", "/api/uploads/"+u.ID, nil, nil), 404)
	u = f.file()
	token := f.share(u, "", 0, 0)
	f.status(f.req("DELETE", "/api/files/"+u.FileID, nil, nil), 204)
	f.status(f.req("GET", "/api/download/"+token, nil, nil), 404)
	if _, e := os.Stat(filepath.Join(f.cfg.StoragePath, u.FileID)); !os.IsNotExist(e) {
		t.Fatal("file not deleted")
	}
}
func TestSecurityAndDiskProtection(t *testing.T) {
	f := setup(t)
	u := f.init("private", 4)
	r := f.req("DELETE", "/api/uploads/"+u.ID, nil, map[string]string{"X-CSRF-Token": "wrong"})
	f.status(r, 403)
	f.status(f.req("DELETE", "/api/uploads/"+u.ID, nil, map[string]string{"Origin": "https://evil.test"}), 403)
	f.status(f.req("GET", "/api/files", nil, map[string]string{"Cookie": ""}), 401)
	f.a.cfg.ReservedBytes = 1 << 60
	f.status(f.json("POST", "/api/uploads", map[string]any{"name": "huge", "size": 1}), 507)
	f.a.cfg.ReservedBytes = 0
	f.status(f.json("POST", "/api/uploads", map[string]any{"name": "huge", "size": 1 << 31}), 413)
	for i := 0; i < 9; i++ {
		f.json("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong"})
	}
	f.status(f.json("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong"}), 429)
}
func TestOwnerIsolation(t *testing.T) {
	f := setup(t)
	u := f.file()
	f.status(f.json("POST", "/api/users", map[string]string{"username": "alice", "password": "alice-test-passphrase", "role": "user"}), 201)
	f.cookie = nil
	r := f.json("POST", "/api/auth/login", map[string]string{"username": "alice", "password": "alice-test-passphrase"})
	f.status(r, 200)
	var b struct {
		CSRF string `json:"csrfToken"`
	}
	json.Unmarshal(r.Body.Bytes(), &b)
	f.csrf = b.CSRF
	f.cookie = r.Result().Cookies()[0]
	f.status(f.req("GET", "/api/uploads/"+u.ID, nil, nil), 404)
	f.status(f.req("DELETE", "/api/files/"+u.FileID, nil, nil), 404)
	f.status(f.json("POST", "/api/files/"+u.FileID+"/shares", map[string]any{}), 404)
	f.status(f.req("GET", "/api/users", nil, nil), 403)
}
func TestCleanupRetention(t *testing.T) {
	f := setup(t)
	u := f.init("expired", 4)
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.a.db.Exec("UPDATE uploads SET expires_at=? WHERE id=?", unixNow()-1, u.ID)
	file := f.file()
	f.a.db.Exec("UPDATE files SET retain_owner=0,delete_at=? WHERE id=?", unixNow()-1, file.FileID)
	if e := f.a.cleanExpired(); e != nil {
		t.Fatal(e)
	}
	f.status(f.req("GET", "/api/uploads/"+u.ID, nil, nil), 404)
	if _, e := os.Stat(filepath.Join(f.cfg.StoragePath, file.FileID)); !os.IsNotExist(e) {
		t.Fatal("retention cleanup failed")
	}
}
func TestCorruptChunkRecovery(t *testing.T) {
	f := setup(t)
	u := f.init("corrupt", 4)
	f.status(f.chunk(u, 0, "abcd"), 204)
	if e := os.WriteFile(filepath.Join(f.cfg.ChunkPath, u.ID, "0.part"), []byte("xxxx"), 0600); e != nil {
		t.Fatal(e)
	}
	f.a.db.Exec("UPDATE uploads SET status='merging' WHERE id=?", u.ID)
	f.a.db.QueryRow("SELECT owner_id FROM uploads WHERE id=?", u.ID).Scan(&u.OwnerID)
	f.a.merge(u)
	r := f.req("GET", "/api/uploads/"+u.ID, nil, nil)
	f.status(r, 200)
	json.Unmarshal(r.Body.Bytes(), &u)
	if len(u.Uploaded) != 0 || u.Error == "" {
		t.Fatal("corrupt chunk not made resumable")
	}
	f.status(f.chunk(u, 0, "abcd"), 204)
	f.finish(u)
}

func TestCrashMergeRecovery(t *testing.T) {
	f := setup(t)
	u := f.init("recover.bin", 4)
	f.status(f.chunk(u, 0, "abcd"), 204)
	dir := filepath.Join(f.cfg.StoragePath, u.FileID)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"payload", ".assembling"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("unfinished"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	f.a.db.Exec("UPDATE uploads SET status='merging' WHERE id=?", u.ID)
	f.a.Close()
	var e error
	f.a, e = New(f.cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	f.finish(u)
	data, e := os.ReadFile(filepath.Join(dir, "payload"))
	if e != nil || string(data) != "abcd" {
		t.Fatalf("crash recovery: %q %v", data, e)
	}
}
func TestMergePollingDoesNotWaitForDiskLock(t *testing.T) {
	f := setup(t)
	u := f.init("pending.bin", 4)
	f.a.db.Exec("UPDATE uploads SET status='merging' WHERE id=?", u.ID)
	l := f.a.lock(u.ID)
	l.Lock()
	defer l.Unlock()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- f.req("GET", "/api/uploads/"+u.ID, nil, nil) }()
	select {
	case r := <-result:
		f.status(r, 200)
	case <-time.After(time.Second):
		t.Fatal("status polling blocked on merge lock")
	}
}
func TestDownloadResourceLimit(t *testing.T) {
	f := setup(t)
	token := f.share(f.file(), "", 0, 0)
	for i := 0; i < cap(f.a.downloadSlots); i++ {
		f.a.downloadSlots <- struct{}{}
	}
	f.status(f.req("GET", "/api/download/"+token, nil, nil), 429)
	for i := 0; i < cap(f.a.downloadSlots); i++ {
		<-f.a.downloadSlots
	}
	f.status(f.req("GET", "/api/download/"+token, nil, nil), 200)
}
