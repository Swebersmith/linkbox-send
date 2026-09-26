#!/usr/bin/env bash
set -euo pipefail
# Output only. Review, then install as /etc/nginx/snippets/cloudflare-realip.conf.
# Validates fetched CIDRs; never executes downloaded content.
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
curl -fsS --proto '=https' https://www.cloudflare.com/ips-v4 -o "$tmp/v4"
curl -fsS --proto '=https' https://www.cloudflare.com/ips-v6 -o "$tmp/v6"
python3 - "$tmp/v4" "$tmp/v6" <<'PY'
import ipaddress, pathlib, sys
ranges = []
for source in sys.argv[1:]:
    entries = pathlib.Path(source).read_text().splitlines()
    if not entries:
        raise SystemExit('Empty Cloudflare address list')
    ranges.extend(ipaddress.ip_network(entry.strip()) for entry in entries if entry.strip())
print('# Generated from Cloudflare official IP lists; refresh when their ranges change.')
for network in ranges:
    print(f'set_real_ip_from {network};')
print('real_ip_header CF-Connecting-IP;')
print('real_ip_recursive on;')
PY
