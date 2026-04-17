-- solver.lua v2 — Lua port of DirectSolver (internal/captcha/direct.go).
-- Solves VK captchaNotRobot via API calls with browser-like telemetry.
local SOLVER_VERSION = "v2-hot-update-test"

-- Config helpers with hardcoded fallbacks.
local function get_ua()
    if config and config.captcha and config.captcha.direct_solver then
        local ua = config.captcha.direct_solver.user_agent
        if ua and ua ~= "" then return ua end
    end
    return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
end

local function get_api_version()
    if config and config.captcha then
        local v = config.captcha.api_version
        if v and v ~= "" then return v end
    end
    return "5.131"
end

local function get_checkbox_answer()
    if config and config.captcha then
        local a = config.captcha.checkbox_answer
        if a and a ~= "" then return a end
    end
    return "e30="
end

local function get_debug_info()
    if config and config.captcha then
        local d = config.captcha.debug_info_fallback
        if d and d ~= "" then return d end
    end
    return "8526f575cd75b95c7974b2ed50c7d67ed07f71048b48f88c64ed9cb498c0942d"
end

-- VK captcha API POST with required headers.
local function vk_captcha_post(ua, method, params)
    local api_ver = get_api_version()
    local endpoint = "https://api.vk.com/method/" .. method .. "?v=" .. api_ver
    local headers = {
        ["User-Agent"] = ua,
        ["Origin"] = "https://id.vk.com",
        ["Referer"] = "https://id.vk.com/",
    }
    local resp = http.post_form(endpoint, headers, params)
    if resp.status ~= 200 then
        error(method .. ": HTTP " .. tostring(resp.status))
    end
    return resp.body
end

-- Generate cursor movement data (matches generateSliderCursor in Go).
local function generate_cursor()
    local points = {}

    -- Start near slider area.
    local x = 500 + math.random(200)
    local y = 400 + math.random(200)
    table.insert(points, {x = x, y = y})

    -- Move towards slider handle.
    local target_x = 580 + math.random(40)
    local target_y = 830 + math.random(20)
    local steps = 5 + math.random(5)
    for i = 1, steps do
        local px = x + math.floor((target_x - x) * i / steps) + math.random(6) - 3
        local py = y + math.floor((target_y - y) * i / steps) + math.random(6) - 3
        table.insert(points, {x = px, y = py})
    end

    -- Drag slider right.
    local drag_steps = 10 + math.random(15)
    local cx, cy = target_x, target_y
    for _ = 1, drag_steps do
        cx = cx + 5 + math.random(15)
        cy = cy + math.random(4) - 2
        table.insert(points, {x = cx, y = cy})
    end

    -- Final hold positions.
    local hold = 2 + math.random(3)
    for _ = 1, hold do
        table.insert(points, {x = cx + math.random(2), y = cy + math.random(2)})
    end

    return points
end

-- Generate device info (matches generateDeviceInfo in Go).
local function generate_device()
    local screens = {
        {w = 1920, h = 1080},
        {w = 2560, h = 1440},
        {w = 1680, h = 1050},
        {w = 1440, h = 900},
    }
    local s = screens[math.random(#screens)]
    local lang = "ru"
    local langs = {"ru"}
    if config and config.captcha and config.captcha.direct_solver then
        if config.captcha.direct_solver.language and config.captcha.direct_solver.language ~= "" then
            lang = config.captcha.direct_solver.language
        end
        if config.captcha.direct_solver.languages then
            langs = config.captcha.direct_solver.languages
        end
    end
    local hw_opts = {8, 12, 16, 24}
    local mem_opts = {8, 16, 32}
    return {
        screenWidth = s.w,
        screenHeight = s.h,
        screenAvailWidth = s.w,
        screenAvailHeight = s.h - 48,
        innerWidth = math.floor(s.w / 2) + math.random(200),
        innerHeight = s.h - 100 - math.random(100),
        devicePixelRatio = 1,
        language = lang,
        languages = langs,
        webdriver = false,
        hardwareConcurrency = hw_opts[math.random(#hw_opts)],
        deviceMemory = mem_opts[math.random(#mem_opts)],
        connectionEffectiveType = "4g",
        notificationsPermission = "denied",
    }
end

-- Generate browser fingerprint (MD5 of random bytes).
local function generate_browser_fp()
    local data = crypto.random_bytes(32)
    return crypto.md5(data)
end

-- Generate random ad fingerprint (21 chars).
local function random_adfp()
    local chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
    local buf = {}
    for i = 1, 21 do
        local idx = math.random(#chars)
        buf[i] = chars:sub(idx, idx)
    end
    return table.concat(buf)
end

-- Generate connection RTT array (11 entries, same base value).
local function generate_rtt()
    local base = 50 + math.random(100)
    local rtt = {}
    for i = 1, 11 do
        rtt[i] = base
    end
    return rtt
end

-- Generate connection downlink array (11 entries, same base value).
local function generate_downlink()
    local base = 5.0 + math.random(15)
    local dl = {}
    for i = 1, 11 do
        dl[i] = base
    end
    return dl
end

-- Fetch captcha page and extract PoW params + captcha type.
local function fetch_captcha_page(ua, redirect_uri)
    local resp = http.get(redirect_uri, {["User-Agent"] = ua})
    local body = resp.body

    local data = {
        captcha_type = nil,
        slider_settings = nil,
        pow_input = nil,
        pow_difficulty = 2, -- default
    }

    -- Extract show_captcha_type.
    local m = re.match([["show_captcha_type"\s*:\s*"([^"]+)"]], body)
    if m then
        data.captcha_type = m[1]
    end

    -- Extract slider settings: {"type":"slider","settings":"<value>"}.
    m = re.match([["type"\s*:\s*"slider"\s*,\s*"settings"\s*:\s*"([^"]+)"]], body)
    if m then
        data.slider_settings = m[1]:gsub("\\/", "/")
    end

    -- Extract PoW input.
    m = re.match([[powInput\s*=\s*"([^"]+)"]], body)
    if m then
        data.pow_input = m[1]
    end

    -- Extract PoW difficulty.
    m = re.match([[difficulty\s*=\s*(%d+)]], body)
    if m then
        data.pow_difficulty = tonumber(m[1]) or 2
    end

    return data
end

-- Compute proof-of-work hash.
-- crypto.pow_solve now matches Go's computeProofOfWork exactly:
-- nonce is decimal int, difficulty is hex-zero count, returns the hash.
local function compute_pow(pow_input, difficulty)
    if not pow_input or pow_input == "" or difficulty <= 0 then
        return ""
    end
    return crypto.pow_solve(pow_input, difficulty)
end

-----------------------------------------------------------------------
-- Main entry point.
-----------------------------------------------------------------------
function solve(challenge)
    local ua = get_ua()
    local adfp = random_adfp()

    -- Parse session_token and domain from redirect_uri.
    local parts = url.parse(challenge.redirect_uri)
    local session_token = parts.raw_query.session_token
    if not session_token or session_token == "" then
        error("no session_token in redirect_uri")
    end
    local domain = parts.raw_query.domain
    if not domain or domain == "" then
        domain = "vk.com"
    end

    -- Step 0: Fetch captcha page for PoW params and captcha type.
    log.info("lua-solver: fetching captcha page", "sid", challenge.captcha_sid)
    local page = fetch_captcha_page(ua, challenge.redirect_uri)

    -- Step 1: captchaNotRobot.settings
    log.debug("captcha direct: settings")
    local settings_resp = vk_captcha_post(ua, "captchaNotRobot.settings", {
        session_token = session_token,
        domain = domain,
        adFp = adfp,
        access_token = "",
    })
    log.debug("captcha direct: settings response", "resp", settings_resp)

    -- Step 2: Slider or checkbox answer.
    local answer = get_checkbox_answer()
    if page.captcha_type == "slider" and page.slider_settings and page.slider_settings ~= "" then
        log.debug("captcha direct: getContent (slider)")
        local content_resp = vk_captcha_post(ua, "captchaNotRobot.getContent", {
            session_token = session_token,
            domain = domain,
            adFp = adfp,
            captcha_settings = page.slider_settings,
            access_token = "",
        })

        if native and native.solve_slider then
            answer = native.solve_slider(content_resp)
            log.debug("captcha direct: slider solved via native")
        else
            error("slider solving requires native module")
        end
    else
        log.debug("captcha direct: checkbox mode", "captchaType", page.captcha_type or "nil")
    end

    -- Simulate delay (sensor collection + user solving): 1.5-3.5s.
    time.sleep(1500 + math.random(2000))

    -- Step 3: captchaNotRobot.componentDone
    log.debug("captcha direct: componentDone")
    local device = generate_device()
    local browser_fp = generate_browser_fp()

    vk_captcha_post(ua, "captchaNotRobot.componentDone", {
        session_token = session_token,
        domain = domain,
        adFp = adfp,
        browser_fp = browser_fp,
        device = json.encode(device),
        access_token = "",
    })

    -- Simulate user interaction delay: 0.5-1.5s.
    time.sleep(500 + math.random(1000))

    -- Step 4: captchaNotRobot.check
    log.debug("captcha direct: check")
    local cursor = generate_cursor()
    local rtt = generate_rtt()
    local downlink = generate_downlink()
    local hash = compute_pow(page.pow_input, page.pow_difficulty)

    log.info("captcha direct: submitting check",
        "ua", ua,
        "type", page.captcha_type or "nil",
        "pow_difficulty", page.pow_difficulty,
        "domain", domain)

    local check_body = vk_captcha_post(ua, "captchaNotRobot.check", {
        session_token = session_token,
        domain = domain,
        adFp = adfp,
        accelerometer = "[]",
        gyroscope = "[]",
        motion = "[]",
        cursor = json.encode(cursor),
        taps = "[]",
        connectionRtt = json.encode(rtt),
        connectionDownlink = json.encode(downlink),
        browser_fp = browser_fp,
        hash = hash,
        answer = answer,
        debug_info = get_debug_info(),
        access_token = "",
    })

    log.info("captcha direct: check response", "body", check_body)

    -- Parse response.
    local result = json.decode(check_body)
    if result.error then
        error("check error " .. tostring(result.error.error_code) .. ": " .. (result.error.error_msg or ""))
    end
    if not result.response or not result.response.success_token or result.response.success_token == "" then
        if result.response and result.response.show_captcha_type and result.response.show_captcha_type ~= "" then
            error("captcha check failed (type=" .. result.response.show_captcha_type .. ", status=" .. (result.response.status or "") .. ")")
        end
        error("no success_token in check response: " .. check_body)
    end

    -- Step 5: endSession (best effort).
    log.debug("captcha direct: endSession")
    pcall(function()
        vk_captcha_post(ua, "captchaNotRobot.endSession", {
            session_token = session_token,
            domain = domain,
            adFp = adfp,
            access_token = "",
        })
    end)

    log.info("lua-solver: captcha solved successfully", "sid", challenge.captcha_sid, "type", page.captcha_type or "checkbox")
    return result.response.success_token
end
