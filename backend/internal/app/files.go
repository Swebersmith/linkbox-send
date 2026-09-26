package app

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"
)

func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	search := query.Get("search")
	if len(search) > 255 {
		problem(w, 400, "search too long")
		return
	}
	sorts := map[string]string{"newest": "f.created_at DESC", "oldest": "f.created_at ASC", "name": "f.original_name COLLATE NOCASE ASC", "largest": "f.file_size DESC", "smallest": "f.file_size ASC"}
	sort := sorts[query.Get("sort")]
	if sort == "" {
		sort = sorts["newest"]
	}
	page, _ := strconv.Atoi(query.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	limit := 50
	var total int
	if e := a.db.QueryRow("SELECT COUNT(*) FROM files WHERE owner_id=? AND instr(lower(original_name),lower(?))>0", auth(r).User.ID, search).Scan(&total); e != nil {
		a.internal(w, "file count", e)
		return
	}
	rows, e := a.db.Query(`SELECT f.id,f.original_name,f.file_size,f.mime_type,f.sha256,f.created_at,f.status,f.retain_owner,f.delete_at,(SELECT COUNT(*) FROM shares s WHERE s.file_id=f.id AND s.revoked=0 AND (s.expires_at IS NULL OR s.expires_at>?) AND (s.max_downloads=0 OR s.download_count<s.max_downloads)) FROM files f WHERE f.owner_id=? AND instr(lower(f.original_name),lower(?))>0 ORDER BY `+sort+",f.id LIMIT ? OFFSET ?", unixNow(), auth(r).User.ID, search, limit, (page-1)*limit)
	if e != nil {
		a.internal(w, "list files", e)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, mime, hash, status string
		var size, created, shares int64
		var retain bool
		var deleteAt sql.NullInt64
		if e = rows.Scan(&id, &name, &size, &mime, &hash, &created, &status, &retain, &deleteAt, &shares); e != nil {
			a.internal(w, "file scan", e)
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "size": size, "mimeType": mime, "sha256": hash, "createdAt": iso(created), "shareCount": shares, "status": status, "retainOwner": retain, "deleteAt": nullableTime(deleteAt)})
	}
	if e = rows.Err(); e != nil {
		a.internal(w, "file rows", e)
		return
	}
	jsonOut(w, 200, map[string]any{"files": out, "total": total, "page": page, "pageSize": limit})
}
func (a *App) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("fileId")
	if !uuidRE.MatchString(id) {
		problem(w, 400, "invalid file ID")
		return
	}
	l := a.lock(id)
	l.Lock()
	defer l.Unlock()
	var owner string
	e := a.db.QueryRow("SELECT owner_id FROM files WHERE id=?", id).Scan(&owner)
	if errors.Is(e, sql.ErrNoRows) || e == nil && owner != auth(r).User.ID {
		problem(w, 404, "file not found")
		return
	}
	if e != nil {
		a.internal(w, "delete lookup", e)
		return
	}
	if e = a.removeFile(id); e != nil {
		a.internal(w, "delete file", e)
		return
	}
	w.WriteHeader(204)
}
func (a *App) removeFile(id string) error {
	// Keep a tombstone before touching the filesystem so cleanup can retry any partial failure.
	if _, e := a.db.Exec("UPDATE files SET status='deleting' WHERE id=?", id); e != nil {
		return e
	}
	if e := a.files.RemoveAll(id); e != nil {
		return e
	}
	if e := syncDirectory(a.files, "."); e != nil {
		return e
	}
	tx, e := a.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("DELETE FROM files WHERE id=?", id); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM uploads WHERE file_id=? AND status='complete'", id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	a.log.Info("file deleted", "file_id", id)
	return nil
}
func (a *App) setRetention(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Retain   bool    `json:"retainOwner"`
		DeleteAt *string `json:"deleteAt"`
	}
	if !decode(w, r, &in) {
		return
	}
	var deleteAt any
	if in.DeleteAt != nil {
		t, e := time.Parse(time.RFC3339, *in.DeleteAt)
		if e != nil || t.Before(time.Now()) {
			problem(w, 400, "deleteAt must be a future RFC3339 time")
			return
		}
		deleteAt = t.Unix()
	}
	if !in.Retain && deleteAt == nil {
		problem(w, 400, "automatic deletion requires deleteAt")
		return
	}
	l := a.lock(r.PathValue("fileId"))
	l.Lock()
	defer l.Unlock()
	res, e := a.db.Exec("UPDATE files SET retain_owner=?,delete_at=? WHERE id=? AND owner_id=? AND status='ready'", in.Retain, deleteAt, r.PathValue("fileId"), auth(r).User.ID)
	if e != nil {
		a.internal(w, "retention", e)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		problem(w, 404, "file not found")
		return
	}
	w.WriteHeader(204)
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	total, available, e := diskSpace(a.cfg.StoragePath)
	if e != nil {
		a.internal(w, "storage stats", e)
		return
	}
	owner := auth(r).User.ID
	day := time.Now().UTC().Truncate(24 * time.Hour).Unix()
	var size, count, uploading, up, down, reserved int64
	queries := []struct {
		sql  string
		args []any
		dest []any
	}{
		{"SELECT COALESCE(SUM(file_size),0),COUNT(*) FROM files WHERE owner_id=?", []any{owner}, []any{&size, &count}},
		{"SELECT COUNT(*) FROM uploads WHERE owner_id=? AND status!='complete'", []any{owner}, []any{&uploading}},
		{"SELECT COALESCE(SUM(bytes_received),0) FROM upload_logs WHERE owner_id=? AND created_at>=?", []any{owner, day}, []any{&up}},
		{"SELECT COALESCE(SUM(bytes_sent),0) FROM download_logs WHERE owner_id=? AND created_at>=?", []any{owner, day}, []any{&down}},
		{"SELECT COALESCE(SUM(2*file_size-(SELECT COALESCE(SUM(chunk_size),0) FROM upload_chunks WHERE upload_id=uploads.id)),0) FROM uploads WHERE status!='complete'", nil, []any{&reserved}},
	}
	for _, q := range queries {
		if e = a.db.QueryRow(q.sql, q.args...).Scan(q.dest...); e != nil {
			a.internal(w, "dashboard", e)
			return
		}
	}
	jsonOut(w, 200, map[string]any{"totalStorage": total, "usedStorage": total - available, "availableStorage": available, "reservedStorage": a.cfg.ReservedBytes, "uploadReservations": reserved, "myFileBytes": size, "fileCount": count, "activeUploads": uploading, "todayUploaded": up, "todayDownloaded": down, "trafficTimezone": "UTC"})
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, map[string]any{"appUrl": a.cfg.AppURL, "chunkSize": a.cfg.ChunkSize, "uploadConcurrency": a.cfg.Concurrency, "uploadExpireHours": int(a.cfg.UploadExpiry.Hours()), "maxFileSize": a.cfg.MaxFileSize, "reservedBytes": a.cfg.ReservedBytes, "maxUploadsPerUser": a.cfg.MaxUploadsPerUser, "directUploadEnabled": false})
}
