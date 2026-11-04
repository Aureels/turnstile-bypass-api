#!/usr/bin/env bash
# Refresh proxies.txt from proxifly/free-proxy-list.
# Usage: ./scripts/refresh-proxies.sh [output_file]
set -euo pipefail

OUT="${1:-proxies.txt}"
BASE="https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

for proto in http https socks4 socks5; do
  echo "fetching $proto ..." >&2
  curl -fsSL --max-time 120 "$BASE/$proto/data.txt" >> "$TMP"
done

# keep valid scheme://host:port lines, drop duplicates, preserve order
awk 'NF && $0 !~ /^#/ && $0 ~ /^(http|https|socks4|socks4a|socks5|socks5h):\/\/[^ ]+$/' "$TMP" \
  | awk '!seen[$0]++' > "$OUT"

echo "wrote $(wc -l < "$OUT") proxies to $OUT" >&2
echo "hot reload: curl -X POST http://localhost:8080/proxies/reload" >&2
