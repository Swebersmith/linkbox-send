CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE,
 password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), created_at INTEGER NOT NULL
);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf_token TEXT NOT NULL, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL
);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE files (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), original_name TEXT NOT NULL,
 storage_path TEXT NOT NULL, file_size INTEGER NOT NULL CHECK(file_size>=0), mime_type TEXT NOT NULL,
 sha256 TEXT NOT NULL, created_at INTEGER NOT NULL, retain_owner INTEGER NOT NULL DEFAULT 1,
 delete_at INTEGER, status TEXT NOT NULL DEFAULT 'ready' CHECK(status IN ('ready','deleting'))
);
CREATE INDEX files_owner_created ON files(owner_id,created_at);
CREATE INDEX files_delete ON files(status,delete_at);
CREATE TABLE uploads (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), original_name TEXT NOT NULL,
 file_size INTEGER NOT NULL CHECK(file_size>=0), mime_type TEXT NOT NULL, chunk_size INTEGER NOT NULL,
 total_chunks INTEGER NOT NULL, status TEXT NOT NULL CHECK(status IN ('uploading','merging','complete')),
 file_id TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX uploads_owner ON uploads(owner_id,status);
CREATE INDEX uploads_expiry ON uploads(expires_at);
CREATE TABLE upload_chunks (
 upload_id TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE, chunk_index INTEGER NOT NULL,
 chunk_size INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(upload_id,chunk_index)
);
CREATE TABLE shares (
 id TEXT PRIMARY KEY, file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE, token TEXT NOT NULL UNIQUE,
 password_hash TEXT NOT NULL DEFAULT '', expires_at INTEGER, max_downloads INTEGER NOT NULL DEFAULT 0 CHECK(max_downloads>=0),
 download_count INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX shares_file ON shares(file_id);
CREATE INDEX shares_expiry ON shares(expires_at);
CREATE TABLE share_grants (
 token_hash TEXT PRIMARY KEY, share_id TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL
);
CREATE INDEX share_grants_expiry ON share_grants(expires_at);
CREATE TABLE download_logs (
 id TEXT PRIMARY KEY, share_id TEXT REFERENCES shares(id) ON DELETE SET NULL, file_id TEXT NOT NULL,
 owner_id TEXT NOT NULL, bytes_sent INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL
);
CREATE INDEX download_logs_date ON download_logs(owner_id,created_at);
CREATE TABLE upload_logs (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL, bytes_received INTEGER NOT NULL, created_at INTEGER NOT NULL
);
CREATE INDEX upload_logs_date ON upload_logs(owner_id,created_at);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
