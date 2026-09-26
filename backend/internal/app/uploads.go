package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Upload struct {
	ID        string `json:"uploadId"`
	OwnerID   string `json:"-"`
	Name      string `json:"fileName"`
	Size      int64  `json:"fileSize"`
	MIME      string `json:"mimeType"`
	ChunkSize int64  `json:"chunkSize"`
	Total     int    `json:"totalChunks"`
	Status    string `json:"status"`
	FileID    string `json:"fileId"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
	Uploaded  []int  `json:"uploadedChunks"`
	Error     string `json:"error,omitempty"`
	Expires   int64  `json:"-"`
}

const uploadSelect = `SELECT id,owner_id,original_name,file_size,mime_type,chunk_size,total_chunks,status,file_id,created_at,expires_at,error FROM uploads`

type scanner interface{ Scan(...any) error }

func scanUpload(row scanner) (Upload, error) {
	var u Upload
	var created int64
	e := row.Scan(&u.ID, &u.OwnerID, &u.Name, &u.Size, &u.MIME, &u.ChunkSize, &u.Total, &u.Status, &u.FileID, &created, &u.Expires, &u.Error)
	u.CreatedAt = iso(created)
	u.ExpiresAt = iso(u.Expires)
	u.Uploaded = []int{}
	return u, e
}
func (a *App) readUpload(id, owner string) (Upload, error) {
	return scanUpload(a.db.QueryRow(uploadSelect+" WHERE id=? AND owner_id=?", id, owner))
}
func (a *App) uploadFor(w http.ResponseWriter, r *http.Request) (Upload, bool) {
	id := r.PathValue("uploadId")
	if !uuidRE.MatchString(id) {
		problem(w, 400, "invalid upload ID")
		return Upload{}, false
	}
	u, e := a.readUpload(id, auth(r).User.ID)
	if errors.Is(e, sql.ErrNoRows) {
		problem(w, 404, "upload not found")
		return u, false
	}
	if e != nil {
		a.internal(w, "upload lookup", e)
		return u, false
	}
	if u.Expires <= unixNow() && u.Status != "complete" && u.Status != "merging" {
		problem(w, 410, "upload expired")
		return u, false
	}
	return u, true
}
func (a *App) uploadedChunks(u Upload) ([]int, error) {
	rows, e := a.db.Query("SELECT chunk_index,chunk_size FROM upload_chunks WHERE upload_id=? ORDER BY chunk_index", u.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var index int
		var size int64
		if e = rows.Scan(&index, &size); e != nil {
			return nil, e
		}
		st, e := a.chunks.Stat(chunkPath(u.ID, index))
		if e == nil && st.Mode().IsRegular() && st.Size() == size {
			out = append(out, index)
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
	}
	return out, rows.Err()
}
func chunkPath(id string, index int) string { return filepath.Join(id, strconv.Itoa(index)+".part") }
func chunkSize(u Upload, index int) int64 {
	if index == u.Total-1 {
		return u.Size - int64(index)*u.ChunkSize
	}
	return u.ChunkSize
}
func (a *App) createUpload(w http.ResponseWriter, r *http.Request) {
	if !a.rates.allow("upload-init:"+auth(r).User.ID, 60, time.Hour) {
		problem(w, 429, "too many upload requests")
		return
	}
	var in struct {
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		MIME      string `json:"mimeType"`
		ChunkSize int64  `json:"chunkSize"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validName(in.Name) {
		problem(w, 400, "invalid file name")
		return
	}
	if in.Size < 0 || in.Size > a.cfg.MaxFileSize {
		problem(w, 413, "file exceeds MAX_FILE_SIZE")
		return
	}
	if in.ChunkSize != 0 && in.ChunkSize != a.cfg.ChunkSize {
		problem(w, 400, "chunkSize must match the server setting")
		return
	}
	if in.MIME == "" {
		in.MIME = "application/octet-stream"
	}
	if _, _, e := mime.ParseMediaType(in.MIME); e != nil || len(in.MIME) > 255 {
		problem(w, 400, "invalid MIME type")
		return
	}
	a.capacityMu.Lock()
	defer a.capacityMu.Unlock()
	var active, mine int
	var outstanding int64
	e := a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN owner_id=? THEN 1 ELSE 0 END),0),COALESCE(SUM(2*file_size-(SELECT COALESCE(SUM(chunk_size),0) FROM upload_chunks WHERE upload_id=uploads.id)),0) FROM uploads WHERE status!='complete'`, auth(r).User.ID).Scan(&active, &mine, &outstanding)
	if e != nil {
		a.internal(w, "upload reservation", e)
		return
	}
	if active >= a.cfg.MaxActiveUploads || mine >= a.cfg.MaxUploadsPerUser {
		problem(w, 429, "active upload limit reached")
		return
	}
	_, free, e := diskSpace(a.cfg.StoragePath)
	if e != nil {
		a.internal(w, "disk space", e)
		return
	}
	if free-a.cfg.ReservedBytes-outstanding < 2*in.Size {
		problem(w, 507, "insufficient storage, including merge workspace and active reservations")
		return
	}
	now := unixNow()
	u := Upload{ID: uuid(), OwnerID: auth(r).User.ID, Name: in.Name, Size: in.Size, MIME: in.MIME, ChunkSize: a.cfg.ChunkSize, Total: int((in.Size + a.cfg.ChunkSize - 1) / a.cfg.ChunkSize), Status: "uploading", FileID: uuid(), CreatedAt: iso(now), ExpiresAt: iso(now + int64(a.cfg.UploadExpiry.Seconds())), Uploaded: []int{}}
	if e = a.chunks.Mkdir(u.ID, 0700); e != nil {
		a.internal(w, "create chunk directory", e)
		return
	}
	_, e = a.db.Exec(`INSERT INTO uploads(id,owner_id,original_name,file_size,mime_type,chunk_size,total_chunks,status,file_id,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,'uploading',?,?,?,?)`, u.ID, u.OwnerID, u.Name, u.Size, u.MIME, u.ChunkSize, u.Total, u.FileID, now, now, now+int64(a.cfg.UploadExpiry.Seconds()))
	if e != nil {
		a.chunks.RemoveAll(u.ID)
		a.internal(w, "create upload", e)
		return
	}
	a.log.Info("upload created", "upload_id", u.ID, "user_id", u.OwnerID, "size", u.Size)
	jsonOut(w, 201, u)
}
func (a *App) getUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := a.uploadFor(w, r)
	if !ok {
		return
	}
	// Polling must remain fast while merge owns the filesystem lock, even for 100 GiB files.
	if u.Status == "merging" || u.Status == "complete" {
		for i := 0; i < u.Total; i++ {
			u.Uploaded = append(u.Uploaded, i)
		}
		jsonOut(w, 200, u)
		return
	}
	var e error
	if u.Status != "complete" {
		u.Uploaded, e = a.uploadedChunks(u)
	}
	if e != nil {
		a.internal(w, "list chunks", e)
		return
	}
	jsonOut(w, 200, u)
}
func (a *App) listUploads(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query(uploadSelect+" WHERE owner_id=? ORDER BY created_at DESC LIMIT 200", auth(r).User.ID)
	if e != nil {
		a.internal(w, "list uploads", e)
		return
	}
	defer rows.Close()
	out := []Upload{}
	for rows.Next() {
		u, e := scanUpload(rows)
		if e != nil {
			a.internal(w, "upload scan", e)
			return
		}
		out = append(out, u)
	}
	if e = rows.Err(); e != nil {
		a.internal(w, "upload rows", e)
		return
	}
	jsonOut(w, 200, out)
}
func (a *App) putChunk(w http.ResponseWriter, r *http.Request) {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/octet-stream" {
		problem(w, 415, "expected application/octet-stream")
		return
	}
	select {
	case a.chunkSlots <- struct{}{}:
		defer func() { <-a.chunkSlots }()
	default:
		w.Header().Set("Retry-After", "2")
		problem(w, 429, "upload concurrency limit reached")
		return
	}
	l := a.lock(r.PathValue("uploadId"))
	l.RLock()
	defer l.RUnlock()
	u, ok := a.uploadFor(w, r)
	if !ok {
		return
	}
	if u.Status != "uploading" {
		problem(w, 409, "upload is not writable")
		return
	}
	index, e := strconv.Atoi(r.PathValue("index"))
	if e != nil || index < 0 || index >= u.Total {
		problem(w, 400, "invalid chunk index")
		return
	}
	cl := a.chunkLock(u.ID + ":" + strconv.Itoa(index))
	cl.Lock()
	defer cl.Unlock()
	expected := chunkSize(u, index)
	if r.ContentLength >= 0 && r.ContentLength != expected {
		problem(w, 400, "incorrect chunk size")
		return
	}
	digest := strings.ToLower(r.Header.Get("X-Chunk-SHA256"))
	if digest != "" {
		b, e := hex.DecodeString(digest)
		if e != nil || len(b) != sha256.Size {
			problem(w, 400, "invalid SHA-256")
			return
		}
	}
	_, free, e := diskSpace(a.cfg.ChunkPath)
	if e != nil {
		a.internal(w, "chunk disk space", e)
		return
	}
	if free-a.cfg.ReservedBytes < expected {
		problem(w, 507, "insufficient disk space")
		return
	}
	temp := filepath.Join(u.ID, uuid()+".tmp")
	f, e := a.chunks.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		a.internal(w, "create chunk", e)
		return
	}
	defer a.chunks.Remove(temp)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Minute))
	h := sha256.New()
	n, copyErr := io.CopyBuffer(io.MultiWriter(f, h), http.MaxBytesReader(w, r.Body, expected+1), make([]byte, 256<<10))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil || n != expected {
		a.log.Warn("upload failed", "upload_id", u.ID, "chunk", index, "reason", "chunk size or stream error")
		problem(w, 400, "incomplete or oversized chunk")
		return
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if digest != "" && digest != actual {
		problem(w, 422, "chunk SHA-256 mismatch")
		return
	}
	var previous string
	e = a.db.QueryRow("SELECT sha256 FROM upload_chunks WHERE upload_id=? AND chunk_index=?", u.ID, index).Scan(&previous)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		a.internal(w, "chunk lookup", e)
		return
	}
	if e == nil && previous != actual {
		problem(w, 409, "chunk already contains different data; cancel and start a new upload")
		return
	}
	final := chunkPath(u.ID, index)
	// A prior rename may have survived a crash before its DB commit. Replacement is safe under this chunk lock.
	if e = a.chunks.Rename(temp, final); e != nil {
		a.internal(w, "commit chunk file", e)
		return
	}
	if e = syncDirectory(a.chunks, u.ID); e != nil {
		a.internal(w, "sync chunk directory", e)
		return
	}
	tx, e := a.db.Begin()
	if e != nil {
		a.internal(w, "chunk transaction", e)
		return
	}
	defer tx.Rollback()
	now := unixNow()
	if _, e = tx.Exec(`INSERT INTO upload_chunks(upload_id,chunk_index,chunk_size,sha256,created_at) VALUES(?,?,?,?,?) ON CONFLICT(upload_id,chunk_index) DO UPDATE SET chunk_size=excluded.chunk_size,sha256=excluded.sha256`, u.ID, index, n, actual, now); e == nil {
		_, e = tx.Exec("UPDATE uploads SET updated_at=?,expires_at=?,error='' WHERE id=?", now, now+int64(a.cfg.UploadExpiry.Seconds()), u.ID)
	}
	if e == nil {
		_, e = tx.Exec("INSERT INTO upload_logs(id,owner_id,bytes_received,created_at) VALUES(?,?,?,?)", uuid(), u.OwnerID, n, now)
	}
	if e != nil {
		a.internal(w, "save chunk", e)
		return
	}
	if e = tx.Commit(); e != nil {
		a.internal(w, "commit chunk", e)
		return
	}
	a.log.Info("chunk uploaded", "upload_id", u.ID, "chunk", index, "bytes", n)
	w.WriteHeader(204)
}
func (a *App) completeUpload(w http.ResponseWriter, r *http.Request) {
	l := a.lock(r.PathValue("uploadId"))
	l.Lock()
	defer l.Unlock()
	u, ok := a.uploadFor(w, r)
	if !ok {
		return
	}
	if u.Status == "complete" {
		jsonOut(w, 200, u)
		return
	}
	if u.Status == "merging" {
		jsonOut(w, 202, u)
		return
	}
	indexes, e := a.uploadedChunks(u)
	if e != nil {
		a.internal(w, "check chunks", e)
		return
	}
	present := map[int]bool{}
	for _, i := range indexes {
		present[i] = true
	}
	missing := []int{}
	for i := 0; i < u.Total; i++ {
		if !present[i] {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		jsonOut(w, 409, map[string]any{"error": "missing chunks", "missingChunks": missing})
		return
	}
	if _, e = a.db.Exec("UPDATE uploads SET status='merging',error='',updated_at=? WHERE id=?", unixNow(), u.ID); e != nil {
		a.internal(w, "schedule merge", e)
		return
	}
	u.Status = "merging"
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.mergeSlots <- struct{}{}:
			defer func() { <-a.mergeSlots }()
		case <-a.ctx.Done():
			return
		}
		a.merge(u)
	}()
	jsonOut(w, 202, u)
}
func (a *App) merge(u Upload) {
	l := a.lock(u.ID)
	l.Lock()
	defer l.Unlock()
	// Cancellation may have removed this queued upload while another file was merging.
	fresh, e := a.readUpload(u.ID, u.OwnerID)
	if e != nil || fresh.Status != "merging" {
		return
	}
	e = a.mergeFile(u)
	if e != nil {
		a.log.Error("upload failed", "upload_id", u.ID, "error", e)
		if _, dbErr := a.db.Exec("UPDATE uploads SET status='uploading',error='Merge failed; retry completion or resume missing chunks',updated_at=?,expires_at=? WHERE id=?", unixNow(), unixNow()+int64(a.cfg.UploadExpiry.Seconds()), u.ID); dbErr != nil {
			a.log.Error("merge recovery", "upload_id", u.ID, "error", dbErr)
		}
	}
}
func (a *App) mergeFile(u Upload) error {
	// Discard only uncommitted output left by an interrupted merge before checking free space.
	var committed int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM files WHERE id=?", u.FileID).Scan(&committed); err != nil {
		return err
	}
	if committed != 0 {
		return fmt.Errorf("merge output already committed")
	}
	if err := a.files.RemoveAll(u.FileID); err != nil {
		return err
	}
	_, free, e := diskSpace(a.cfg.StoragePath)
	if e != nil {
		return e
	}
	if free-a.cfg.ReservedBytes < u.Size {
		return fmt.Errorf("insufficient merge disk space")
	}
	if e = a.files.MkdirAll(u.FileID, 0700); e != nil {
		return e
	}
	temp := filepath.Join(u.FileID, ".assembling")
	out, e := a.files.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer out.Close()
	defer a.files.Remove(temp)
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var total int64
	for i := 0; i < u.Total; i++ {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		default:
		}
		var expectedHash string
		if e = a.db.QueryRow("SELECT sha256 FROM upload_chunks WHERE upload_id=? AND chunk_index=?", u.ID, i).Scan(&expectedHash); e != nil {
			return e
		}
		part, e := a.chunks.Open(chunkPath(u.ID, i))
		if e != nil {
			return e
		}
		ch := sha256.New()
		n, copyErr := io.CopyBuffer(io.MultiWriter(out, hash, ch), io.LimitReader(part, chunkSize(u, i)+1), buffer)
		part.Close()
		if copyErr != nil {
			return copyErr
		}
		if n != chunkSize(u, i) || hex.EncodeToString(ch.Sum(nil)) != expectedHash {
			// Corrupt chunks become missing so the next resume can repair them.
			if e = a.chunks.Remove(chunkPath(u.ID, i)); e != nil {
				return e
			}
			if _, e = a.db.Exec("DELETE FROM upload_chunks WHERE upload_id=? AND chunk_index=?", u.ID, i); e != nil {
				return e
			}
			return fmt.Errorf("chunk integrity failure at %d", i)
		}
		total += n
	}
	if total != u.Size {
		return fmt.Errorf("merged file size mismatch")
	}
	if e = out.Sync(); e != nil {
		return e
	}
	if e = out.Close(); e != nil {
		return e
	}
	if e = a.files.Rename(temp, filePath(u.FileID)); e != nil {
		return e
	}
	if e = syncDirectory(a.files, u.FileID); e != nil {
		return e
	}
	if e = syncDirectory(a.files, "."); e != nil {
		return e
	}
	tx, e := a.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	now := unixNow()
	if _, e = tx.Exec(`INSERT INTO files(id,owner_id,original_name,storage_path,file_size,mime_type,sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`, u.FileID, u.OwnerID, u.Name, filepath.Join(a.cfg.StoragePath, filePath(u.FileID)), u.Size, u.MIME, hex.EncodeToString(hash.Sum(nil)), now); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE uploads SET status='complete',updated_at=?,expires_at=?,error='' WHERE id=?", now, now+int64(a.cfg.UploadExpiry.Seconds()), u.ID); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM upload_chunks WHERE upload_id=?", u.ID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = a.chunks.RemoveAll(u.ID); e != nil {
		a.log.Warn("cleanup", "upload_id", u.ID, "error", e)
	}
	a.log.Info("upload completed", "upload_id", u.ID, "file_id", u.FileID, "bytes", total)
	return nil
}
func (a *App) cancelUpload(w http.ResponseWriter, r *http.Request) {
	l := a.lock(r.PathValue("uploadId"))
	l.Lock()
	defer l.Unlock()
	// Expired uploads remain cancellable, including their retained reservations.
	id := r.PathValue("uploadId")
	if !uuidRE.MatchString(id) {
		problem(w, 400, "invalid upload ID")
		return
	}
	u, e := a.readUpload(id, auth(r).User.ID)
	if errors.Is(e, sql.ErrNoRows) {
		w.WriteHeader(204)
		return
	}
	if e != nil {
		a.internal(w, "cancel lookup", e)
		return
	}
	if e = a.removeUpload(u); e != nil {
		a.internal(w, "cancel upload", e)
		return
	}
	w.WriteHeader(204)
}
func (a *App) removeUpload(u Upload) error {
	if e := a.chunks.RemoveAll(u.ID); e != nil {
		return e
	}
	if u.Status != "complete" {
		if e := a.files.RemoveAll(u.FileID); e != nil {
			return e
		}
	}
	_, e := a.db.Exec("DELETE FROM uploads WHERE id=?", u.ID)
	if e == nil {
		a.log.Info("cleanup", "upload_id", u.ID, "action", "upload removed")
	}
	return e
}
