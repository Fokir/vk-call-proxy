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

  old_size=$(python3 -c "
import json, sys
m = json.load(open('$MANIFEST'))
s = m.get('scripts',{}).get('$name',{})
print(s.get('size',0))
" 2>/dev/null || echo "0")

  if [[ "$sha" != "$old_sha" || "$size" != "$old_size" ]]; then
    echo "  $name: sha256=$sha size=$size (was: $old_sha / $old_size)"
    UPDATED=1
  fi
done

if [[ $UPDATED -eq 0 ]]; then
  echo "All hashes match — manifest is up to date."
  exit 0
fi

# Rewrite manifest with updated hashes
python3 -c "
import json, os, hashlib

bundled = '$BUNDLED_DIR'
mpath = os.path.join(bundled, 'manifest.json')
with open(mpath) as f:
    m = json.load(f)

for name in list(m.get('scripts', {})):
    fpath = os.path.join(bundled, name)
    if not os.path.isfile(fpath):
        continue
    data = open(fpath, 'rb').read()
    m['scripts'][name]['sha256'] = hashlib.sha256(data).hexdigest()
    m['scripts'][name]['size'] = len(data)

with open(mpath, 'w') as f:
    json.dump(m, f, indent=2)
    f.write('\n')
"

echo "Manifest updated: $MANIFEST"
echo "Don't forget to rebuild binaries (go build / gomobile bind)."
