#!/usr/bin/env bash
# Sync script files from hot-scripts/ (single source of truth) into
# internal/scripts/bundled/ and regenerate manifest.json with correct
# SHA256 hashes. Run before go build / gomobile bind.
#
# Usage: bash scripts/resign-bundled.sh
set -euo pipefail

SRC_DIR="hot-scripts"
DST_DIR="internal/scripts/bundled"

if [[ ! -d "$SRC_DIR" ]]; then
  echo "FATAL: $SRC_DIR not found" >&2
  exit 1
fi

mkdir -p "$DST_DIR"

# Copy script files from hot-scripts/ to bundled/ (skip manifest.json).
COPIED=0
for file in "$SRC_DIR"/*; do
  name=$(basename "$file")
  [[ "$name" == "manifest.json" ]] && continue
  [[ ! -f "$file" ]] && continue

  if [[ ! -f "$DST_DIR/$name" ]] || ! cmp -s "$file" "$DST_DIR/$name"; then
    cp "$file" "$DST_DIR/$name"
    echo "  $name: synced"
    COPIED=1
  fi
done

# Remove bundled files that no longer exist in hot-scripts/.
for file in "$DST_DIR"/*; do
  name=$(basename "$file")
  [[ "$name" == "manifest.json" ]] && continue
  [[ ! -f "$file" ]] && continue
  if [[ ! -f "$SRC_DIR/$name" ]]; then
    rm "$file"
    echo "  $name: removed (deleted from $SRC_DIR)"
    COPIED=1
  fi
done

if [[ $COPIED -eq 0 ]]; then
  echo "All files match — bundled/ is up to date."
fi

# Regenerate manifest.json with correct hashes.
python3 -c "
import json, os, hashlib, glob, datetime

bundled = '$DST_DIR'
mpath = os.path.join(bundled, 'manifest.json')

m = {}
if os.path.isfile(mpath):
    with open(mpath) as f:
        m = json.load(f)

m.setdefault('scripts', {})

# Update existing + auto-add new script files.
seen = set()
for fpath in sorted(glob.glob(os.path.join(bundled, '*'))):
    name = os.path.basename(fpath)
    if name == 'manifest.json' or not os.path.isfile(fpath):
        continue
    seen.add(name)
    data = open(fpath, 'rb').read()
    m['scripts'][name] = {
        'sha256': hashlib.sha256(data).hexdigest(),
        'size': len(data),
        'url': f'bundled://{name}',
    }

# Remove entries for deleted files.
for name in list(m['scripts']):
    if name not in seen:
        del m['scripts'][name]

m['version'] = 'bundled-' + datetime.date.today().strftime('%Y.%m.%d')
m['published_at'] = datetime.datetime.now(datetime.UTC).strftime('%Y-%m-%dT%H:%M:%SZ')
m['signature'] = ''

with open(mpath, 'w') as f:
    json.dump(m, f, indent=2)
    f.write('\n')
"

echo "Manifest updated: $DST_DIR/manifest.json"
