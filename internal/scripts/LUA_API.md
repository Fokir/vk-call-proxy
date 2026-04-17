# Lua Captcha Solver API Reference

Solver scripts run in an embedded Lua 5.1 VM (`gopher-lua`). Each solve gets
a fresh VM — no state persists between calls.

## Entry Point

```lua
function solve(challenge)
    -- challenge.redirect_uri  (string)
    -- challenge.captcha_sid   (string)
    -- challenge.captcha_ts    (number)
    -- challenge.captcha_img   (string)
    return success_token  -- string
end
```

Return `success_token` on success. Call `error("reason")` on failure.

---

## http — Network Requests

Shared cookie jar per solve session. Timeout inherited from Go context (30s).

```lua
-- GET
local resp = http.get(url, {["User-Agent"] = "...", ["Accept"] = "..."})
-- resp.status  (int)
-- resp.body    (string)
-- resp.headers (table: name → value)

-- POST raw body
local resp = http.post(url, headers, body_string)

-- POST application/x-www-form-urlencoded
local resp = http.post_form(url, headers, {field1 = "val1", field2 = "val2"})

-- POST application/json
local resp = http.post_json(url, headers, {key = "value"})
```

---

## img — Image Processing

Images are opaque Go objects (Lua userdata). Cannot be inspected directly —
use accessor functions.

### Loading

```lua
local image = img.load(bytes)            -- from raw bytes (PNG/JPEG/WebP)
local image = img.decode_base64(str)     -- from base64 string (with or without data URI prefix)
```

### Properties

```lua
local w = img.width(image)
local h = img.height(image)
local r, g, b, a = img.pixel(image, x, y)  -- 0-based coordinates, values 0-255
```

### Transforms

```lua
local cropped  = img.crop(image, x, y, w, h)
local resized  = img.resize(image, w, h)     -- bilinear interpolation
local gray     = img.grayscale(image)
local edges    = img.edge_detect(image)       -- Canny edge detection
local diffimg  = img.diff(img1, img2)         -- absolute pixel difference
local rotated  = img.rotate(image, degrees)   -- arbitrary angle, bicubic
local flipped  = img.flip(image, true)        -- true=horizontal, false=vertical
local binary   = img.threshold(image, value)  -- pixels > value → white, else black
local blurred  = img.blur(image, radius)      -- Gaussian blur
```

### Analysis

```lua
local x, y, score = img.template_match(haystack, needle)
-- Finds best match position. score: 0.0 (perfect) to 1.0 (no match)

local similarity = img.ssim(img1, img2)
-- Structural similarity index: 1.0 = identical, 0.0 = completely different

local hist = img.histogram(image)
-- {r = {[0]=count, [1]=count, ...255}, g = {...}, b = {...}}

local count = img.color_count(image, r, g, b, tolerance)
-- Count pixels within tolerance of (r,g,b). Tolerance: max per-channel diff.

local colors = img.dominant_colors(image, n)
-- Returns n dominant colors: {{r=.., g=.., b=.., pct=..}, ...}

local regions = img.connected_components(image)
-- Returns list of bounding boxes: {{x=.., y=.., w=.., h=..}, ...}

local mask = img.alpha_mask(image)
-- Extract alpha channel as grayscale image
```

### Output

```lua
local bytes = img.encode(image, "png")   -- "png" or "jpeg"
img.draw_rect(image, x, y, w, h, {255, 0, 0})  -- draw red rectangle (mutates)
```

---

## crypto — Cryptography & Hashing

### Hashing

```lua
local hex = crypto.sha256(data)           -- SHA-256 → hex string
local hex = crypto.sha1(data)             -- SHA-1 → hex string
local hex = crypto.md5(data)              -- MD5 → hex string
local hex = crypto.hmac_sha256(key, data) -- HMAC-SHA256 → hex string
local hex = crypto.hmac_sha1(key, data)   -- HMAC-SHA1 → hex string
local int = crypto.crc32(data)            -- CRC32 checksum
```

### Encoding

```lua
local str  = crypto.base64_encode(data)
local data = crypto.base64_decode(str)
local hex  = crypto.hex_encode(data)
local data = crypto.hex_decode(hex)
```

### Encryption

```lua
local ct = crypto.aes_encrypt(key, iv, plaintext)   -- AES-128-CBC, PKCS7 padding
local pt = crypto.aes_decrypt(key, iv, ciphertext)
```

### Utilities

```lua
local bytes = crypto.random_bytes(n)               -- cryptographically secure
local nonce = crypto.pow_solve(prefix, difficulty)  -- SHA-256 proof-of-work
-- Finds nonce where sha256(prefix .. nonce) has `difficulty` leading zero bits

local result = crypto.xor(data1, data2)            -- byte-wise XOR (repeats shorter)
```

---

## fp — Fingerprint & Telemetry

Generate plausible browser fingerprint data for anti-bot evasion.

```lua
-- Human-like mouse movement (Bezier curve + jitter)
local points = fp.mouse_path(x1, y1, x2, y2, num_points)
-- Returns: {{x=.., y=..}, {x=.., y=..}, ...}

-- Realistic device fingerprint
local device = fp.device_info(seed)
-- Returns: {screenWidth, screenHeight, screenAvailWidth, screenAvailHeight,
--           innerWidth, innerHeight, devicePixelRatio, language, languages,
--           hardwareConcurrency, deviceMemory, connectionEffectiveType,
--           notificationsPermission, webdriver}

-- Browser fingerprint hash (MD5 of random bytes, deterministic for seed)
local hash = fp.browser_fp(seed)

-- Connection telemetry arrays
local rtt = fp.connection_rtt(count)           -- {50, 55, 48, ...} ms
local downlink = fp.connection_downlink(count) -- {10.5, 11.2, ...} Mbps

-- Random ad fingerprint string
local adfp = fp.random_adfp()                 -- 21-char random string

-- Click event with realistic offsets
local click = fp.mouse_click(x, y)
-- Returns: {x=.., y=.., offsetX=.., offsetY=.., timestamp=..}

-- Scroll events
local events = fp.scroll_events(duration_ms)
-- Returns: {{deltaX=.., deltaY=.., timestamp=..}, ...}
```

---

## json — JSON Encode/Decode

```lua
local table = json.decode(str)        -- JSON string → Lua table
local str   = json.encode(table)      -- Lua table → JSON string
local str   = json.encode_pretty(table)  -- with indentation (debug)
```

Nested tables, arrays, strings, numbers, booleans, nil all supported.

---

## re — Regular Expressions

Go `regexp` syntax (RE2). NOT Lua patterns.

```lua
local captures = re.match(pattern, str)
-- First match. Returns table of captures, or nil.
-- captures[0] = full match, captures[1] = first group, etc.

local all = re.find_all(pattern, str)
-- All matches. Returns table of capture tables.

local result = re.replace(pattern, str, replacement)
-- Replace all matches. $1, $2 for capture groups in replacement.
```

---

## url — URL Utilities

```lua
local encoded = url.encode(str)       -- percent-encoding
local decoded = url.decode(str)
local parts   = url.parse(str)
-- {scheme="https", host="api.vk.com", path="/method/...",
--  query="v=5.131", fragment="", raw_query={v="5.131"}}
```

---

## time — Timing

```lua
time.sleep(milliseconds)       -- blocks Lua execution
local ms = time.now()          -- Unix epoch milliseconds (int)
local ms = time.since(start)   -- milliseconds since `start` (from time.now())
```

---

## log — Logging

Output goes to Go `slog` with `component=lua-solver`.

```lua
log.debug("fetching page", "url", url)
log.info("captcha type detected", "type", captcha_type)
log.warn("retry failed", "attempt", i, "err", err_msg)
```

Key-value pairs after message are optional structured fields.

---

## config — Hot-Reload Configuration

Read-only table populated from `vk-config.json` before `solve()` is called.

```lua
-- VK API params
config.vk.api_version              -- "5.275"
config.vk.ws.app_version           -- "1.1"
config.vk.user_agents              -- {"Mozilla/...", "Mozilla/...", ...}

-- Captcha params
config.captcha.api_version         -- "5.131"
config.captcha.checkbox_answer     -- "e30="
config.captcha.debug_info_fallback -- "8526f575..."

-- DirectSolver params (now used by solver.lua)
config.captcha.direct_solver.user_agent  -- "Mozilla/...Chrome/148..."
config.captcha.direct_solver.language    -- "ru"
config.captcha.direct_solver.languages   -- {"ru"}

-- Stealth params
config.captcha.stealth.languages   -- {"ru-RU", "ru", "en-US", "en"}
config.captcha.stealth.platform    -- "Win32"
```

---

## native — Go-Optimized Functions

Optional. Check availability before calling:

```lua
if native and native.solve_slider then
    local answer = native.solve_slider(content_json)
else
    -- pure Lua fallback
    local answer = my_lua_slider_solver(content_json)
end
```

### Available native functions

| Function | Args | Returns | Description |
|---|---|---|---|
| `native.solve_slider(json)` | getContent JSON response | base64 answer string | Edge-matching slider solver (current DirectSolver algorithm) |

Additional native functions are added to Go code as needed. Lua scripts
should always provide a fallback path.

---

## Globals

| Name | Type | Description |
|---|---|---|
| `config` | table | Read-only config from `vk-config.json` |
| `challenge` | table | Passed as argument to `solve()` |

Standard Lua libraries available: `string`, `table`, `math`, `os.clock` (time only, no exec).
`io`, `os.execute`, `loadfile`, `dofile` are **disabled** for security.

---

## Example: Minimal Checkbox Solver

```lua
function solve(challenge)
    local ua = config.captcha.direct_solver.user_agent
    local headers = {["User-Agent"] = ua, ["Origin"] = "https://id.vk.com"}

    -- Parse session_token from redirect_uri
    local parts = url.parse(challenge.redirect_uri)
    local session_token = parts.raw_query.session_token
    local domain = parts.raw_query.domain or "vk.com"

    -- Fetch captcha page for PoW params
    local page = http.get(challenge.redirect_uri, {["User-Agent"] = ua})
    local pow_input = re.match('"powInput":"([^"]+)"', page.body)
    local pow_diff = tonumber(re.match('"powDifficulty":(%d+)', page.body) or "2")

    local api_ver = config.captcha.api_version
    local adfp = fp.random_adfp()
    local browser_fp = fp.browser_fp(nil)

    -- Step 1: settings
    http.post_form("https://api.vk.com/method/captchaNotRobot.settings?v=" .. api_ver,
        headers, {session_token = session_token, domain = domain, adFp = adfp, access_token = ""})

    -- Step 2: componentDone
    local device = fp.device_info(nil)
    http.post_form("https://api.vk.com/method/captchaNotRobot.componentDone?v=" .. api_ver,
        headers, {
            session_token = session_token, domain = domain, adFp = adfp,
            browser_fp = browser_fp, device = json.encode(device), access_token = ""
        })

    -- Simulate interaction delay
    time.sleep(1500 + math.random(2000))

    -- Step 3: check
    local hash = crypto.pow_solve(pow_input[1], pow_diff)
    local cursor = fp.mouse_path(100, 300, 200, 300, 30)

    local resp = http.post_form("https://api.vk.com/method/captchaNotRobot.check?v=" .. api_ver,
        headers, {
            session_token = session_token, domain = domain, adFp = adfp,
            accelerometer = "[]", gyroscope = "[]", motion = "[]",
            cursor = json.encode(cursor), taps = "[]",
            connectionRtt = json.encode(fp.connection_rtt(11)),
            connectionDownlink = json.encode(fp.connection_downlink(11)),
            browser_fp = browser_fp,
            hash = hash,
            answer = config.captcha.checkbox_answer,
            debug_info = config.captcha.debug_info_fallback,
            access_token = ""
        })

    local result = json.decode(resp.body)
    if result.response and result.response.success_token ~= "" then
        return result.response.success_token
    end

    error("captcha check failed: " .. (result.response and result.response.status or "unknown"))
end
```
