#!/usr/bin/env bash
set -euo pipefail
# Generates real random bytes; does not rely on a sparse filesystem allocation.
# Usage: ./scripts/generate-test-file.sh ./test-1g.bin 1024
output=${1:-./test-1g.bin}
mib=${2:-1024}
[[ "$mib" =~ ^[1-9][0-9]*$ ]] || { echo 'Size must be a positive integer in MiB.' >&2; exit 1; }
[[ ! -e "$output" ]] || { echo "Refusing to overwrite: $output" >&2; exit 1; }
dd if=/dev/urandom of="$output" bs=1048576 count="$mib" status=progress
sha256sum "$output"
