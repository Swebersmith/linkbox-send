#!/usr/bin/env bash
set -euo pipefail
umask 077
project=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$project"
backup_base=${1:-"$project/backups"}
mkdir -p -- "$backup_base"
backup_base=$(cd -- "$backup_base" && pwd -P)
destination="$backup_base/linkbox-$(date -u +%Y%m%dT%H%M%SZ)"
[[ ! -e "$destination" ]] || { echo 'Backup destination already exists.' >&2; exit 1; }
mkdir -m 700 -- "$destination"
was_running=$(docker compose ps --status running -q backend)
restart() { if [[ -n "$was_running" ]]; then docker compose start backend >/dev/null; fi; }
trap restart EXIT
echo 'Stopping backend to snapshot database and files consistently…'
docker compose stop -t 45 backend
docker compose run --rm --no-deps --user 0:0 --cap-add DAC_OVERRIDE --cap-add CHOWN --cap-add FOWNER --entrypoint /bin/sh \
  -v "$destination:/backup" backend -ec '
    test -f "$DATABASE_PATH"
    sqlite3 "$DATABASE_PATH" ".backup /backup/linkbox.db"
    test "$(sqlite3 /backup/linkbox.db "PRAGMA integrity_check;")" = "ok"
    tar -C "$STORAGE_PATH" -czf /backup/files.tar.gz .
    chmod 600 /backup/linkbox.db /backup/files.tar.gz
  '
printf 'LinkBox Send backup v1\nCreated UTC: %s\nFiles and SQLite snapshot; chunks excluded.\n' "$(date -u +%FT%TZ)" > "$destination/manifest.txt"
(cd "$destination" && sha256sum linkbox.db files.tar.gz manifest.txt > SHA256SUMS)
echo "Backup ready: $destination"
