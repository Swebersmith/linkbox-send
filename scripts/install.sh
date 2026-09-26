#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Install LinkBox Send on an Ubuntu VPS with Docker, Nginx and Let's Encrypt.

Usage: sudo bash scripts/install.sh --domain send.example.com --email admin@example.com
       [--admin-username admin]

Before running, point the domain at this VPS and allow inbound TCP 80 and 443.
For the first certificate, use Cloudflare DNS Only until installation finishes;
then switch the record to Proxy ON and use Full (strict) TLS.
The install directory is the repository containing this script.
EOF
}

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '\n==> %s\n' "$*"; }

domain='' email='' admin_username='admin' generated_password=''
while (($#)); do
  case "$1" in
    --domain|--email|--admin-username)
      (($# >= 2)) || fail "missing value for $1"
      case "$1" in
        --domain) domain="${2,,}" ;;
        --email) email="$2" ;;
        --admin-username) admin_username="$2" ;;
      esac
      shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[[ -n "$domain" && -n "$email" ]] || { usage >&2; exit 2; }
[[ ${#domain} -le 253 && "$domain" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] || fail 'invalid domain'
[[ "$email" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || fail 'invalid email'
[[ "$admin_username" =~ ^[A-Za-z0-9_.-]{3,64}$ ]] || fail 'admin username must be 3–64 ASCII letters, digits, _, - or .'
(( EUID == 0 )) || fail 'run with sudo'
[[ -r /etc/os-release ]] || fail 'this installer requires Ubuntu'
# shellcheck disable=SC1091
. /etc/os-release
[[ "${ID:-}" == ubuntu ]] || fail 'this installer requires Ubuntu'
command -v flock >/dev/null || fail 'flock is required (util-linux)'
exec 9>/var/lock/linkbox-send-install.lock
flock -n 9 || fail 'another LinkBox Send installation is running'

project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
[[ -f "$project_dir/docker-compose.yml" && -f "$project_dir/.env.example" ]] || fail 'run the installer from a complete repository checkout'
cd "$project_dir"

on_exit() {
  result=$?
  if [[ -n "$generated_password" ]]; then
    printf '\nInitial admin username: %s\nInitial admin password: %s\n' "$admin_username" "$generated_password"
    printf 'Save this password now. It is also in %s/.env, readable by root only.\n' "$project_dir"
  fi
  if ((result != 0)); then
    printf 'Installation stopped. Existing .env and ./data were kept. Fix the error and rerun the same command.\n' >&2
  fi
}
trap on_exit EXIT

read_env() {
  awk -v wanted="$1" 'index($0,"=") { key=substr($0,1,index($0,"=")-1); if(key==wanted) value=substr($0,index($0,"=")+1) } END { print value }' .env
}

if [[ -e .env || -L .env ]]; then
  [[ -f .env && ! -L .env ]] || fail '.env must be a regular file'
  [[ "$(read_env APP_URL)" == "https://$domain" ]] || fail 'existing .env APP_URL differs from --domain; installer did not overwrite it'
  [[ -n "$(read_env ADMIN_PASSWORD)" && -n "$(read_env SESSION_SECRET)" ]] || fail 'existing .env needs ADMIN_PASSWORD and SESSION_SECRET'
  log 'Keeping existing .env and data'
else
  command -v openssl >/dev/null || { apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y openssl; }
  generated_password="$(openssl rand -hex 24)"
  session_secret="$(openssl rand -hex 32)"
  awk -v url="https://$domain" -v admin="$admin_username" -v password="$generated_password" -v secret="$session_secret" '
    BEGIN { values["APP_URL"]=url; values["ADMIN_USERNAME"]=admin; values["ADMIN_PASSWORD"]=password; values["SESSION_SECRET"]=secret }
    index($0,"=") { key=substr($0,1,index($0,"=")-1); if(key in values) { print key "=" values[key]; next } }
    { print }
  ' .env.example > .env
  chmod 600 .env
  log 'Created .env with random admin password and session secret'
fi
chmod 600 .env

app_port="$(read_env APP_PORT)"
app_bind="$(read_env APP_BIND)"
body_size="$(read_env NGINX_UPLOAD_BODY_SIZE)"
[[ "$app_port" =~ ^[0-9]{1,5}$ ]] && ((10#$app_port >= 1 && 10#$app_port <= 65535)) || fail 'APP_PORT must be 1–65535'
[[ "$app_bind" == 127.0.0.1 ]] || fail 'APP_BIND must be 127.0.0.1 for the host Nginx installer'
[[ "$body_size" == 40m || "$body_size" == 72m ]] || fail 'NGINX_UPLOAD_BODY_SIZE must be 40m or 72m'

log 'Installing system packages'
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg nginx certbot openssl

if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
  log 'Installing Docker Engine and Compose from the official Docker apt repository'
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  architecture="$(dpkg --print-architecture)"
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' "$architecture" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.list
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker

log 'Building and starting LinkBox Send'
docker compose config --quiet
docker compose up -d --build
curl -fsS --retry 30 --retry-connrefused --retry-delay 2 "http://127.0.0.1:$app_port/health" >/dev/null || fail 'application health check failed; inspect docker compose logs backend'

site=/etc/nginx/sites-available/linkbox-send
enabled=/etc/nginx/sites-enabled/linkbox-send
webroot=/var/www/linkbox-acme
cert_dir="/etc/letsencrypt/live/$domain"
if [[ -e "$site" || -L "$site" ]]; then
  [[ -f "$site" && ! -L "$site" ]] || fail "$site is not a regular file"
  cp -p "$site" "$site.before-install-$(date -u +%Y%m%dT%H%M%SZ)"
fi
install -d -m 0755 "$webroot/.well-known/acme-challenge"
if [[ -e "$enabled" || -L "$enabled" ]]; then
  [[ -L "$enabled" && "$(readlink -f "$enabled")" == "$site" ]] || fail "$enabled already exists and is not this installer's symlink"
else
  ln -s "$site" "$enabled"
fi

if [[ ! -s "$cert_dir/fullchain.pem" || ! -s "$cert_dir/privkey.pem" ]]; then
  log 'Requesting a Let’s Encrypt certificate with HTTP-01'
  cat > "$site" <<EOF
server {
    listen 80;
    server_name $domain;
    access_log off;
    location ^~ /.well-known/acme-challenge/ {
        root $webroot;
        default_type text/plain;
        try_files \$uri =404;
    }
    location / { return 404; }
}
EOF
  nginx -t
  systemctl enable --now nginx
  systemctl reload nginx
fi
certbot certonly --webroot -w "$webroot" -d "$domain" -m "$email" --agree-tos --no-eff-email --non-interactive --keep-until-expiring || fail 'certificate request failed; verify DNS and inbound port 80, or temporarily set Cloudflare DNS Only'

log 'Configuring Nginx TLS reverse proxy'
sed -e "s/send\.example\.com/$domain/g" \
    -e "s#/etc/ssl/linkbox/origin\.pem#$cert_dir/fullchain.pem#g" \
    -e "s#/etc/ssl/linkbox/origin\.key#$cert_dir/privkey.pem#g" \
    -e "s/127\.0\.0\.1:8080/127.0.0.1:$app_port/g" \
    -e "s/client_max_body_size 40m;/client_max_body_size $body_size;/g" \
    deploy/nginx/host-tls.conf.example > "$site"
nginx -t
systemctl enable --now nginx
systemctl reload nginx
install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
cat > /etc/letsencrypt/renewal-hooks/deploy/linkbox-nginx-reload.sh <<'EOF'
#!/bin/sh
systemctl reload nginx
EOF
chmod 755 /etc/letsencrypt/renewal-hooks/deploy/linkbox-nginx-reload.sh
systemctl enable --now certbot.timer 2>/dev/null || true

log "Installed successfully: https://$domain"
printf 'If using Cloudflare, switch this DNS record to Proxy ON and SSL/TLS to Full (strict).\n'
printf 'Keep TCP 80 reachable for certificate renewal, and TCP 443 for the site.\n'
