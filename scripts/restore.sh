#!/usr/bin/env bash
set -euo pipefail
umask 077
[[ $# -ge 1 ]] || { echo 'Usage: restore.sh BACKUP_DIRECTORY [--replace]' >&2; exit 1; }
project=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source_dir=$(cd -- "$1" && pwd -P)
[[ -f "$source_dir/SHA256SUMS" && -f "$source_dir/linkbox.db" && -f "$source_dir/files.tar.gz" ]] || { echo 'Incomplete backup.' >&2; exit 1; }
(cd "$source_dir" && sha256sum -c SHA256SUMS)
cd "$project"
[[ -f .env ]] || { echo 'Create and configure .env before restoring.' >&2; exit 1; }
target="$project/data"
[[ ! -L "$target" ]] || { echo 'Refusing symlinked data directory.' >&2; exit 1; }
[[ ! -e "$target" || ${2:-} == '--replace' ]] || { echo 'data already exists. Use --replace to preserve it as a dated rollback directory.' >&2; exit 1; }
case "$source_dir/" in "$target/"*) echo 'Backup must be outside the data directory.' >&2; exit 1;; esac
# Validate archive paths and types before changing the existing deployment.
python3 - "$source_dir/files.tar.gz" <<'PY'
import pathlib, sys, tarfile
with tarfile.open(sys.argv[1], 'r:gz') as archive:
    for entry in archive:
        path = pathlib.PurePosixPath(entry.name)
        if path.is_absolute() or '..' in path.parts or not (entry.isfile() or entry.isdir()):
            raise SystemExit('Unsafe backup archive member: ' + entry.name)
PY
docker compose down
if [[ -e "$target" ]]; then
  rollback="$project/data-before-restore-$(date -u +%Y%m%dT%H%M%SZ)"
  [[ ! -e "$rollback" ]] || { echo 'Rollback destination exists.' >&2; exit 1; }
  mv -- "$target" "$rollback"
  echo "Previous data preserved at: $rollback"
fi
mkdir -p "$target/database" "$target/files" "$target/chunks"
cp -- "$source_dir/linkbox.db" "$target/database/linkbox.db"
tar --no-same-owner --no-same-permissions -xzf "$source_dir/files.tar.gz" -C "$target/files"
# Expired sessions and partial uploads must never come back after restoring a backup.
docker compose run --rm --no-deps --user 0:0 --cap-add DAC_OVERRIDE --cap-add CHOWN --cap-add FOWNER --entrypoint /bin/sh backend -ec '
  test "$(sqlite3 "$DATABASE_PATH" "PRAGMA integrity_check;")" = "ok"
  sqlite3 "$DATABASE_PATH" "PRAGMA foreign_keys=ON; BEGIN; DELETE FROM sessions; DELETE FROM share_grants; DELETE FROM upload_chunks; DELETE FROM uploads; COMMIT; PRAGMA wal_checkpoint(TRUNCATE);"
  chown -R 10001:10001 /data
  chmod 700 /data /data/database /data/files /data/chunks
'
docker compose up -d
echo 'Restore complete. Check docker compose ps and sign in again.'
