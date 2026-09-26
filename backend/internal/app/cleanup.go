package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) cleanup() {
	if e := a.cleanExpired(); e != nil {
		a.log.Error("cleanup", "error", e)
	}
}
func (a *App) cleanExpired() error {
	now := unixNow()
	rows, e := a.db.Query(uploadSelect+" WHERE expires_at<=? AND status!='merging'", now)
	if e != nil {
		return e
	}
	uploads := []Upload{}
	for rows.Next() {
		u, e := scanUpload(rows)
		if e != nil {
			rows.Close()
			return e
		}
		uploads = append(uploads, u)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, u := range uploads {
		l := a.lock(u.ID)
		l.Lock()
		fresh, e := a.readUpload(u.ID, u.OwnerID)
		if e == nil && fresh.Expires <= now && fresh.Status != "merging" {
			e = a.removeUpload(fresh)
		}
		l.Unlock()
		if e != nil {
			a.log.Warn("cleanup", "upload_id", u.ID, "error", e)
		}
	}
	rows, e = a.db.Query("SELECT id FROM files WHERE status='deleting' OR (retain_owner=0 AND delete_at<=?)", now)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		l := a.lock(id)
		l.Lock()
		var n int
		e = a.db.QueryRow("SELECT COUNT(*) FROM files WHERE id=? AND (status='deleting' OR (retain_owner=0 AND delete_at<=?))", id, now).Scan(&n)
		if e == nil && n > 0 {
			e = a.removeFile(id)
		}
		l.Unlock()
		if e != nil {
			a.log.Warn("cleanup", "file_id", id, "error", e)
		}
	}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{"DELETE FROM sessions WHERE expires_at<=?", now},
		{"DELETE FROM share_grants WHERE expires_at<=?", now},
		{"UPDATE shares SET revoked=1 WHERE revoked=0 AND expires_at<=?", now},
		{"DELETE FROM upload_logs WHERE created_at<?", now - 90*86400},
		{"DELETE FROM download_logs WHERE created_at<?", now - 90*86400},
	} {
		if _, e = a.db.Exec(q.sql, q.arg); e != nil {
			return e
		}
	}
	if e = a.cleanOrphans(a.chunks, true); e != nil {
		return e
	}
	if e = a.cleanOrphans(a.files, false); e != nil {
		return e
	}
	a.log.Info("cleanup", "status", "finished", "expired_uploads", len(uploads), "files_due", len(ids))
	return nil
}
func (a *App) cleanOrphans(root *os.Root, isChunk bool) error {
	dir, e := root.Open(".")
	if e != nil {
		return e
	}
	entries, e := dir.ReadDir(-1)
	dir.Close()
	if e != nil {
		return e
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !uuidRE.MatchString(id) {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if time.Since(info.ModTime()) < time.Hour {
			continue
		}
		l := a.lock(id)
		l.Lock()
		var count int
		if isChunk {
			e = a.db.QueryRow("SELECT COUNT(*) FROM uploads WHERE id=? AND status!='complete'", id).Scan(&count)
		} else {
			e = a.db.QueryRow("SELECT (SELECT COUNT(*) FROM files WHERE id=?)+(SELECT COUNT(*) FROM uploads WHERE file_id=?)", id, id).Scan(&count)
		}
		if e == nil && count == 0 {
			e = root.RemoveAll(id)
			if e == nil {
				a.log.Info("cleanup", "orphan_id", id)
			}
		}
		if e == nil && isChunk && count > 0 {
			child, err := root.Open(id)
			if err == nil {
				parts, err := child.ReadDir(-1)
				child.Close()
				if err == nil {
					for _, part := range parts {
						if strings.HasSuffix(part.Name(), ".tmp") {
							stat, err := part.Info()
							if err == nil && time.Since(stat.ModTime()) > time.Hour {
								if err = root.Remove(filepath.Join(id, part.Name())); err != nil {
									e = fmt.Errorf("remove stale chunk temp: %w", err)
								}
							}
						}
					}
				}
			}
		}
		l.Unlock()
		if e != nil {
			return e
		}
	}
	return nil
}
