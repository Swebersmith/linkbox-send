package app

import (
	"database/sql"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type share struct {
	ID, FileID, Token, Hash, Name, Owner   string
	Size, Created, FileCreated, Max, Count int64
	Expires                                sql.NullInt64
	Revoked                                int
}

func (a *App) shareByToken(token string) (share, error) {
	var s share
	e := a.db.QueryRow(`SELECT s.id,s.file_id,s.token,s.password_hash,s.expires_at,s.max_downloads,s.download_count,s.created_at,s.revoked,f.original_name,f.file_size,f.created_at,f.owner_id FROM shares s JOIN files f ON f.id=s.file_id WHERE s.token=? AND f.status='ready'`, token).Scan(&s.ID, &s.FileID, &s.Token, &s.Hash, &s.Expires, &s.Max, &s.Count, &s.Created, &s.Revoked, &s.Name, &s.Size, &s.FileCreated, &s.Owner)
	return s, e
}
func (s share) gone() bool {
	return s.Revoked != 0 || (s.Expires.Valid && s.Expires.Int64 <= unixNow()) || (s.Max > 0 && s.Count >= s.Max)
}
func (a *App) publicShareFor(w http.ResponseWriter, r *http.Request) (share, bool) {
	token := r.PathValue("token")
	if !tokenRE.MatchString(token) {
		problem(w, 404, "share not found")
		return share{}, false
	}
	s, e := a.shareByToken(token)
	if errors.Is(e, sql.ErrNoRows) {
		problem(w, 404, "share not found")
		return s, false
	}
	if e != nil {
		a.internal(w, "share lookup", e)
		return s, false
	}
	if s.gone() {
		problem(w, 410, "share expired, revoked or download limit reached")
		return s, false
	}
	return s, true
}
func grantName(s share) string { return "linkbox_share_" + strings.ReplaceAll(s.ID, "-", "") }
func (a *App) hasGrant(r *http.Request, s share) bool {
	if s.Hash == "" {
		return true
	}
	c, e := r.Cookie(grantName(s))
	if e != nil || !tokenRE.MatchString(c.Value) {
		return false
	}
	var n int
	e = a.db.QueryRow("SELECT COUNT(*) FROM share_grants WHERE token_hash=? AND share_id=? AND expires_at>?", a.tokenHash(c.Value), s.ID, unixNow()).Scan(&n)
	return e == nil && n == 1
}
func shareJSON(s share) map[string]any {
	return map[string]any{"id": s.ID, "fileId": s.FileID, "fileName": s.Name, "fileSize": s.Size, "uploadedAt": iso(s.FileCreated), "createdAt": iso(s.Created), "expiresAt": nullableTime(s.Expires), "maxDownloads": s.Max, "downloadCount": s.Count, "passwordProtected": s.Hash != ""}
}
func (a *App) publicShare(w http.ResponseWriter, r *http.Request) {
	s, ok := a.publicShareFor(w, r)
	if !ok {
		return
	}
	if !a.hasGrant(r, s) {
		jsonOut(w, 200, map[string]any{"passwordRequired": true})
		return
	}
	jsonOut(w, 200, shareJSON(s))
}
func (a *App) verifyShare(w http.ResponseWriter, r *http.Request) {
	if !a.rates.allow("share-password-ip:"+a.clientIP(r), 20, 15*time.Minute) || !a.rates.allow("share-password-token:"+r.PathValue("token"), 100, 15*time.Minute) {
		w.Header().Set("Retry-After", "900")
		problem(w, 429, "too many password attempts")
		return
	}
	s, ok := a.publicShareFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.Hash != "" {
		if !a.passwordSlot(w) {
			return
		}
		defer func() { <-a.hashSlots }()
		if len(in.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(s.Hash), []byte(in.Password)) != nil {
			problem(w, 401, "incorrect share password")
			return
		}
	}
	token := randomToken()
	expiry := unixNow() + 900
	if s.Expires.Valid && s.Expires.Int64 < expiry {
		expiry = s.Expires.Int64
	}
	if _, e := a.db.Exec("INSERT INTO share_grants(token_hash,share_id,expires_at) VALUES(?,?,?)", a.tokenHash(token), s.ID, expiry); e != nil {
		a.internal(w, "share grant", e)
		return
	}
	a.cookie(w, grantName(s), token, "/api", int(expiry-unixNow()))
	jsonOut(w, 200, shareJSON(s))
}
func (a *App) createShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("fileId")
	if !uuidRE.MatchString(id) {
		problem(w, 400, "invalid file ID")
		return
	}
	l := a.lock(id)
	l.RLock()
	defer l.RUnlock()
	var found string
	e := a.db.QueryRow("SELECT id FROM files WHERE id=? AND owner_id=? AND status='ready'", id, auth(r).User.ID).Scan(&found)
	if errors.Is(e, sql.ErrNoRows) {
		problem(w, 404, "file not found")
		return
	}
	if e != nil {
		a.internal(w, "share file", e)
		return
	}
	if !a.rates.allow("share-create:"+auth(r).User.ID, 60, time.Hour) {
		problem(w, 429, "too many shares")
		return
	}
	var in struct {
		ExpiresIn int64  `json:"expiresIn"`
		Password  string `json:"password"`
		Max       int64  `json:"maxDownloads"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ExpiresIn != 0 && in.ExpiresIn != 3600 && in.ExpiresIn != 86400 && in.ExpiresIn != 604800 && in.ExpiresIn != 2592000 {
		problem(w, 400, "invalid share lifetime")
		return
	}
	if in.Max < 0 || in.Max > 1000000000 {
		problem(w, 400, "invalid maximum downloads")
		return
	}
	hash := ""
	if in.Password != "" {
		if !a.passwordSlot(w) {
			return
		}
		defer func() { <-a.hashSlots }()
		hash, e = hashPassword(in.Password)
		if e != nil {
			problem(w, 400, e.Error())
			return
		}
	}
	var expires any
	if in.ExpiresIn > 0 {
		expires = unixNow() + in.ExpiresIn
	}
	token, shareID := randomToken(), uuid()
	_, e = a.db.Exec("INSERT INTO shares(id,file_id,token,password_hash,expires_at,max_downloads,created_at) VALUES(?,?,?,?,?,?,?)", shareID, id, token, hash, expires, in.Max, unixNow())
	if e != nil {
		a.internal(w, "create share", e)
		return
	}
	a.log.Info("share created", "share_id", shareID, "file_id", id)
	jsonOut(w, 201, map[string]any{"id": shareID, "token": token, "url": a.cfg.AppURL + "/s/" + token})
}
func (a *App) listShares(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query(`SELECT s.id,s.token,s.password_hash!='',s.expires_at,s.max_downloads,s.download_count,s.created_at,s.revoked FROM shares s JOIN files f ON f.id=s.file_id WHERE f.id=? AND f.owner_id=? ORDER BY s.created_at DESC LIMIT 200`, r.PathValue("fileId"), auth(r).User.ID)
	if e != nil {
		a.internal(w, "list shares", e)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, token string
		var protected bool
		var expiry sql.NullInt64
		var max, count, created int64
		var revoked int
		if e = rows.Scan(&id, &token, &protected, &expiry, &max, &count, &created, &revoked); e != nil {
			a.internal(w, "share scan", e)
			return
		}
		out = append(out, map[string]any{"id": id, "url": a.cfg.AppURL + "/s/" + token, "passwordProtected": protected, "expiresAt": nullableTime(expiry), "maxDownloads": max, "downloadCount": count, "createdAt": iso(created), "revoked": revoked != 0})
	}
	if e = rows.Err(); e != nil {
		a.internal(w, "share rows", e)
		return
	}
	jsonOut(w, 200, out)
}
func (a *App) revokeShare(w http.ResponseWriter, r *http.Request) {
	res, e := a.db.Exec(`UPDATE shares SET revoked=1 WHERE id=? AND file_id IN (SELECT id FROM files WHERE owner_id=?)`, r.PathValue("shareId"), auth(r).User.ID)
	if e != nil {
		a.internal(w, "revoke share", e)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		problem(w, 404, "share not found")
		return
	}
	w.WriteHeader(204)
}

type downloadWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *downloadWriter) Write(b []byte) (int, error) {
	n, e := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, e
}
func (w *downloadWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Limit to one byte range per request, preventing multipart range amplification.
func validRange(value string, size int64) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "" {
		n, e := strconv.ParseInt(parts[1], 10, 64)
		return e == nil && n > 0 && size > 0
	}
	start, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || start < 0 || start >= size {
		return false
	}
	if parts[1] == "" {
		return true
	}
	end, e := strconv.ParseInt(parts[1], 10, 64)
	return e == nil && end >= start
}
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	if !a.rates.allow("download:"+a.clientIP(r), 120, time.Minute) {
		w.Header().Set("Retry-After", "60")
		problem(w, 429, "too many download requests")
		return
	}
	select {
	case a.downloadSlots <- struct{}{}:
		defer func() { <-a.downloadSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		problem(w, 429, "download concurrency limit reached")
		return
	}
	s, ok := a.publicShareFor(w, r)
	if !ok {
		return
	}
	if !a.hasGrant(r, s) {
		problem(w, 401, "share password required")
		return
	}
	l := a.lock(s.FileID)
	l.RLock()
	defer l.RUnlock()
	f, e := a.files.Open(filePath(s.FileID))
	if e != nil {
		problem(w, 404, "file unavailable")
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() != s.Size {
		problem(w, 503, "file storage is inconsistent")
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if !validRange(r.Header.Get("Range"), s.Size) {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(s.Size, 10))
		problem(w, 416, "requested range not satisfiable")
		return
	}
	logID := ""
	if r.Method != "HEAD" {
		// The conditional increment is atomic across concurrent requests and survives restarts.
		tx, e := a.db.Begin()
		if e != nil {
			a.internal(w, "download transaction", e)
			return
		}
		defer tx.Rollback()
		res, e := tx.Exec(`UPDATE shares SET download_count=download_count+1 WHERE id=? AND revoked=0 AND (expires_at IS NULL OR expires_at>?) AND (max_downloads=0 OR download_count<max_downloads)`, s.ID, unixNow())
		if e != nil {
			a.internal(w, "reserve download", e)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			problem(w, 410, "share unavailable")
			return
		}
		logID = uuid()
		if _, e = tx.Exec("INSERT INTO download_logs(id,share_id,file_id,owner_id,created_at) VALUES(?,?,?,?,?)", logID, s.ID, s.FileID, s.Owner, unixNow()); e != nil {
			a.internal(w, "download log", e)
			return
		}
		if e = tx.Commit(); e != nil {
			a.internal(w, "download commit", e)
			return
		}
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.Name}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	a.log.Info("download started", "share_id", s.ID, "file_id", s.FileID, "range", r.Header.Get("Range"))
	dw := &downloadWriter{ResponseWriter: w}
	http.ServeContent(dw, r, s.Name, time.Unix(s.FileCreated, 0), f)
	if logID != "" {
		if _, e = a.db.Exec("UPDATE download_logs SET bytes_sent=? WHERE id=?", dw.bytes, logID); e != nil {
			a.log.Error("download log update", "error", e)
		}
	}
}
