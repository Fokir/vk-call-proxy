#!/usr/bin/env bash
# Recompute SHA256 hashes and sizes in the bundled manifest after editing
# script files. Run this after any change to internal/scripts/bundled/*.
#
# Usage: bash scripts/resign-bundled.sh
set -euo pipefail

BUNDLED_DIR="internal/scripts/bundled"
MANIFEST="$BUNDLED_DIR/manifest.json"

if [[ ! -f "$MANIFEST" ]]; then
  echo "FATAL: $MANIFEST not found" >&2
  exit 1
fi

# Collect script files (everything except manifest.json)
UPDATED=0
for file in "$BUNDLED_DIR"/*; do
  name=$(basename "$file")
  [[ "$name" == "manifest.json" ]] && continue

  sha=$(sha256sum "$file" | awk '{print $1}')
  size=$(wc -c < "$file" | tr -d ' ')

  # Read current values from manifest
  old_sha=$(python3 -c "
import json, sys
m = json.load(open('$MANIFEST'))
s = m.get('scripts',{}).get('$name',{})
print(s.get('sha256',''))
" 2>/dev/null || echo "")

  if [[ -z "$old_sha" ]]; then
    echo "  $name: NEW file (sha256=${sha:0:16}... size=$size)"
    UPDATED=1
  elif [[ "$sha" != "$old_sha" ]]; then
    echo "  $name: CHANGED (sha256=$sha)"
    UPDATED=1
  fi
done

if [[ $UPDATED -eq 0 ]]; then
  echo "All hashes match — manifest is up to date."
fi

# Rewrite manifest with updated hashes and auto-add new files
python3 -c "
import json, os, hashlib, glob

bundled = '$BUNDLED_DIR'
mpath = os.path.join(bundled, 'manifest.json')
with open(mpath) as f:
    m = json.load(f)

if 'scripts' not in m:
    m['scripts'] = {}

# Update existing + auto-add new script files
for fpath in sorted(glob.glob(os.path.join(bundled, '*'))):
    name = os.path.basename(fpath)
    if name == 'manifest.json' or not os.path.isfile(fpath):
        continue
    data = open(fpath, 'rb').read()
    sha = hashlib.sha256(data).hexdigest()
    size = len(data)
    if name not in m['scripts']:
        print(f'  + {name}: NEW file (sha256={sha[:16]}... size={size})')
    m['scripts'][name] = {
        'sha256': sha,
        'size': size,
        'url': f'bundled://{name}',
    }

# Remove entries for deleted files
for name in list(m['scripts']):
    if not os.path.isfile(os.path.join(bundled, name)):
        print(f'  - {name}: REMOVED')
        del m['scripts'][name]

import datetime
m['version'] = 'bundled-' + datetime.date.today().strftime('%Y.%m.%d')
m['published_at'] = datetime.datetime.now(datetime.UTC).strftime('%Y-%m-%dT%H:%M:%SZ')

with open(mpath, 'w') as f:
    json.dump(m, f, indent=2)
    f.write('\n')
"

echo "Manifest updated: $MANIFEST"
echo "Don't forget to rebuild binaries (go build / gomobile bind)."
