#!/usr/bin/env bash
# Deploy debug APK to rooted Android device with config backup/restore.
# Usage: ./scripts/deploy-debug.sh [--skip-aar] [--no-restore]
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PKG="com.callvpn.app.debug"
APK="$PROJECT_ROOT/mobile/android/app/build/outputs/apk/debug/app-debug.apk"
AAR="$PROJECT_ROOT/mobile/android/app/libs/bind.aar"
BACKUP="/tmp/callvpn_backup.xml"
REMOTE_TMP="/data/local/tmp"
PREFS_DIR="/data/data/$PKG/shared_prefs"
PREFS_FILE="$PREFS_DIR/callvpn.xml"

SKIP_AAR=false
NO_RESTORE=false
for arg in "$@"; do
  case "$arg" in
    --skip-aar)  SKIP_AAR=true ;;
    --no-restore) NO_RESTORE=true ;;
  esac
done

# adb wrapper: MSYS_NO_PATHCONV prevents path mangling for remote args.
# For 'adb push', convert the local path to Windows format so adb.exe can find it.
adb_() {
  if [[ "${1:-}" == "push" && -n "${2:-}" && "$2" == /* ]]; then
    local winpath
    winpath="$(cygpath -w "$2" 2>/dev/null || echo "$2")"
    MSYS_NO_PATHCONV=1 adb push "$winpath" "${@:3}"
  else
    MSYS_NO_PATHCONV=1 adb "$@"
  fi
}
su_()  { adb_ shell "su -c '$*'"; }

echo "=== Checking device ==="
adb_ devices | grep -q 'device$' || { echo "ERROR: no device connected"; exit 1; }

# --- Build AAR ---
if [ "$SKIP_AAR" = false ]; then
  echo "=== Building AAR (gomobile bind) ==="
  # Remove stale AAR files that conflict with bind.aar
  find "$PROJECT_ROOT/mobile/android/app/libs/" -name '*.aar' ! -name 'bind.aar' -delete 2>/dev/null || true
  find "$PROJECT_ROOT/mobile/android/app/libs/" -name '*-sources.jar' ! -name 'bind-sources.jar' -delete 2>/dev/null || true
  cd "$PROJECT_ROOT"
  gomobile bind -target android -androidapi 24 \
    -ldflags "-checklinkname=0" \
    -o "$AAR" ./mobile/bind/
  echo "AAR built: $(ls -lh "$AAR" | awk '{print $5}')"
else
  echo "=== Skipping AAR build ==="
fi

# --- Build APK ---
echo "=== Building debug APK ==="
cd "$PROJECT_ROOT/mobile/android"
./gradlew assembleDebug -q
echo "APK built: $(ls -lh "$APK" | awk '{print $5}')"

# --- Backup config ---
HAS_BACKUP=false
if su_ "test -f $PREFS_FILE" 2>/dev/null; then
  echo "=== Backing up config ==="
  adb_ shell "su -c 'cat $PREFS_FILE'" > "$BACKUP"
  SIZE=$(wc -c < "$BACKUP")
  if [ "$SIZE" -gt 10 ]; then
    HAS_BACKUP=true
    echo "Backup: $SIZE bytes"
  else
    echo "WARNING: backup too small ($SIZE bytes), skipping restore"
  fi
else
  echo "=== No existing config to backup ==="
fi

# --- Install APK ---
echo "=== Installing APK ==="
adb_ push "$APK" "$REMOTE_TMP/app-debug.apk"

INSTALL_OUT=$(su_ "pm install -r $REMOTE_TMP/app-debug.apk" 2>&1) || true
if echo "$INSTALL_OUT" | grep -q "INSTALL_FAILED_UPDATE_INCOMPATIBLE"; then
  echo "Signature mismatch — uninstalling old version first"
  su_ "pm uninstall $PKG"
  INSTALL_OUT=$(su_ "pm install $REMOTE_TMP/app-debug.apk" 2>&1)
fi

if echo "$INSTALL_OUT" | grep -q "Success"; then
  echo "Install OK"
else
  echo "ERROR: install failed: $INSTALL_OUT"
  exit 1
fi

# --- Restore config ---
if [ "$HAS_BACKUP" = true ] && [ "$NO_RESTORE" = false ]; then
  echo "=== Restoring config ==="
  NEW_UID=$(su_ "stat -c %u /data/data/$PKG/" | tr -d '\r')
  adb_ push "$BACKUP" "$REMOTE_TMP/callvpn.xml"
  su_ "mkdir -p $PREFS_DIR"
  su_ "cp $REMOTE_TMP/callvpn.xml $PREFS_FILE"
  su_ "chown ${NEW_UID}:${NEW_UID} $PREFS_FILE"
  su_ "chmod 660 $PREFS_FILE"
  echo "Config restored (UID=$NEW_UID)"
fi

# --- Cleanup ---
su_ "rm $REMOTE_TMP/app-debug.apk $REMOTE_TMP/callvpn.xml 2>/dev/null" || true

echo "=== Done ==="
