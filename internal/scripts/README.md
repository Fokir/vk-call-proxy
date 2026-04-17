# Hot-Update Scripts Bundle

## Overview

The scripts bundle system allows updating VK API parameters, captcha solver
config and stealth scripts **without rebuilding** the app. Two copies exist:

| Directory | Purpose | Signed? | Embedded? |
|---|---|---|---|
| `internal/scripts/bundled/` | Compile-time fallback (Go `embed`) | No | Yes |
| `hot-scripts/` | Remote hot-update via GitHub raw URL | Yes (ED25519) | No |

Clients load scripts in this priority:

1. **Local cache** (`var/scripts/current/` on desktop, `/data/data/.../files/scripts/current/` on Android)
2. **Bundled** (compiled into binary via `//go:embed`)
3. **Remote** (polled every 1h from `hot-scripts/` on GitHub)

When a remote update arrives, it replaces the local cache. Next launch loads
from cache (step 1) instead of bundled (step 2).

## Files

### `vk-config.json`

```jsonc
{
  "vk": {
    "api_version": "5.275",        // VK Calls API version
    "ws": { ... },                  // WebSocket signaling params
    "user_agents": [ ... ]          // UA pool for VK API calls
  },
  "captcha": {
    "api_version": "5.131",         // captchaNotRobot API version
    "checkbox_answer": "e30=",      // base64 answer for checkbox captcha
    "debug_info_fallback": "...",   // SHA-256 hash sent in check request
    "direct_solver": {
      "user_agent": "...Chrome/148.0.0.0...",  // UA for DirectSolver HTTP requests
      "language": "ru",             // navigator.language in device fingerprint
      "languages": ["ru"]           // navigator.languages in device fingerprint
    },
    "chromedp_solver": { ... },     // headless Chrome solver config
    "stealth": { ... }              // WebGL/platform fingerprint overrides
  }
}
```

### `stealth.js`

JavaScript injected into headless Chrome before captcha page load.
Overrides `navigator.webdriver`, WebGL fingerprint, etc.

### `manifest.json`

```json
{
  "version": "bundled-2026.04.15",
  "published_at": "2026-04-15T15:18:58Z",
  "scripts": {
    "vk-config.json": {
      "sha256": "<sha256 of file>",
      "size": 2140,
      "url": "bundled://vk-config.json"
    }
  },
  "signature": ""
}
```

- `sha256` and `size` **must match** the actual file, otherwise the bundle fails to load
- `url` is `bundled://` for embedded, or `https://raw.githubusercontent.com/...` for remote
- `signature` is empty for bundled, ED25519 base64 for remote

## How to Update

### Quick: edit config and rebuild

1. Edit `internal/scripts/bundled/vk-config.json` (or `stealth.js`)
2. Run `scripts/resign-bundled.sh` to update manifest hashes
3. Build as usual (`go build`, `gomobile bind`)

### Remote hot-update (no rebuild needed)

1. Edit `hot-scripts/vk-config.json` (or `stealth.js`)
2. Push to `master`
3. CI workflow `scripts-publish.yml` automatically:
   - Computes SHA256 hashes for all files
   - Signs manifest with ED25519 key from `SCRIPTS_SIGNING_KEY` secret
   - Commits signed `hot-scripts/manifest.json`
4. Running clients pick up the update within 1 hour (or on next restart)

### Updating both copies

When changing config values, **update both** `internal/scripts/bundled/` and
`hot-scripts/` to keep them in sync:

```bash
# 1. Edit the config
vim internal/scripts/bundled/vk-config.json
cp internal/scripts/bundled/vk-config.json hot-scripts/vk-config.json

# 2. Resign bundled manifest
bash scripts/resign-bundled.sh

# 3. Commit & push (CI will sign hot-scripts/manifest.json)
git add internal/scripts/bundled/ hot-scripts/
git commit -m "chore(scripts): update vk-config"
git push
```

## Common Gotchas

### Local cache overrides bundled

The scripts manager prefers local cache over bundled embed. If a stale
`var/scripts/current/vk-config.json` exists, your bundled changes won't
take effect. Fix:

```bash
# Desktop
rm -rf var/scripts/current

# Android
adb shell "su -c 'rm -rf /data/data/com.callvpn.app/files/scripts/current'"
```

### Manifest hash mismatch

If you edit a script file but forget to update `manifest.json`, the bundle
loader will reject the file (SHA256 mismatch) and fall back to whatever
was in cache. Always run `scripts/resign-bundled.sh` after editing.

### DirectSolver User-Agent

VK's bot detection is sensitive to the User-Agent sent by DirectSolver.
Key findings:

- `Chrome/148.0.0.0` (fictional future version) **works** - VK doesn't have it in known-bot DB
- `Chrome/135.0.0.0` (real version) **gets BOT** - VK fingerprints real Chrome versions
- Always use a **non-existent future Chrome version** for DirectSolver UA
- The `chromedp_solver.user_agent` can use a real version (it runs actual Chrome)

### captcha.RefreshFunc

When DirectSolver fails (e.g. BOT response), the `captcha_sid` is burned.
ChainSolver calls `RefreshFunc` to obtain a fresh challenge before passing
to the next solver (WebView callback on Android, Chromedp on server).

## Architecture

```
internal/scripts/
  bundled/              # Go embed source
    manifest.json       # hashes + sizes (no signature)
    vk-config.json      # VK API + captcha config
    stealth.js          # Chrome stealth patches
  bundled.go            # //go:embed bundled
  manager.go            # Load priority, background poll, failure rollback
  store.go              # Local cache (current/ + previous/ for rollback)
  fetcher.go            # HTTP download + ETag caching
  verify.go             # ED25519 signature + SHA256 hash verification
  config.go             # Config struct + ResolveConfig()
  vkconfig.go           # VKConfig parser (typed access to vk-config.json)

internal/scriptshook/
  hook.go               # Wires Manager into captcha + vk packages

internal/captcha/
  scripts_provider.go   # captchaUserAgent(), captchaLanguage(), etc.

internal/provider/vk/
  scripts_provider.go   # apiVersion(), userAgents(), etc.

hot-scripts/            # Remote-served copy (signed by CI)
  manifest.json         # ED25519-signed, with GitHub raw URLs
  vk-config.json
  stealth.js
```

## CI Workflow

`.github/workflows/scripts-publish.yml`:
- Triggers on push to `master` when `hot-scripts/**` changes (excluding manifest)
- Signs with `SCRIPTS_SIGNING_KEY` secret (ED25519 private key, base64)
- Commits signed manifest back to master with `[skip ci]`

## Key Generation

```bash
go run ./tools/scripts-sign keygen -out secrets/
# Creates: secrets/scripts-signing.key (private), secrets/scripts-signing.pub (public)
# Add private key content to GitHub repo secret SCRIPTS_SIGNING_KEY
# Set public key as DefaultPublicKey in internal/scripts/config.go
```
