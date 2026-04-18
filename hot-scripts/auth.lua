-- auth.lua — VK/OK authentication flow for obtaining TURN credentials.
-- Replaces the Go HTTP calls in vk.go with hot-updatable Lua logic.
--
-- Entry point: fetch_join_info(args) where args = {join_link, name, token}
-- Returns: table {username, password, urls, ws_endpoint, conv_id, device_idx}
-- On error: error("error message")
--
-- Globals provided by Go runner:
--   config   — parsed vk-config.json
--   args     — {join_link="...", name="...", token=""}
--   captcha  — {solve=function(challenge_table) → result_table}
--   http, json, crypto, log, time, url, re — standard modules

-----------------------------------------------------------------------
-- Config accessors with compiled-in fallbacks.
-----------------------------------------------------------------------

local function vk_client_id()
    if config and config.vk and config.vk.client_id and config.vk.client_id ~= "" then
        return config.vk.client_id
    end
    return "6287487"
end

local function vk_client_secret()
    if config and config.vk and config.vk.client_secret and config.vk.client_secret ~= "" then
        return config.vk.client_secret
    end
    return "QbYic1K3lEV5kTGiqlq2"
end

local function vk_api_version()
    if config and config.vk and config.vk.api_version and config.vk.api_version ~= "" then
        return config.vk.api_version
    end
    return "5.275"
end

local function vk_anon_token_url()
    if config and config.vk and config.vk.anon_token_url and config.vk.anon_token_url ~= "" then
        return config.vk.anon_token_url
    end
    return "https://login.vk.com/?act=get_anonym_token"
end

local function vk_api_url()
    if config and config.vk and config.vk.api_url and config.vk.api_url ~= "" then
        return config.vk.api_url
    end
    return "https://api.vk.ru/method"
end

local function vk_api_url_com()
    if config and config.vk and config.vk.api_url_com and config.vk.api_url_com ~= "" then
        return config.vk.api_url_com
    end
    return "https://api.vk.com/method"
end

local function ok_app_key()
    if config and config.ok and config.ok.app_key and config.ok.app_key ~= "" then
        return config.ok.app_key
    end
    return "CGMMEJLGDIHBABABA"
end

local function ok_api_url()
    if config and config.ok and config.ok.api_url and config.ok.api_url ~= "" then
        return config.ok.api_url
    end
    return "https://calls.okcdn.ru/fb.do"
end

local function random_user_agent()
    if config and config.vk and config.vk.user_agents and #config.vk.user_agents > 0 then
        return config.vk.user_agents[math.random(#config.vk.user_agents)]
    end
    return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
end

-----------------------------------------------------------------------
-- Utility: generate UUID v4 from random bytes.
-----------------------------------------------------------------------

local function generate_uuid()
    local b = {string.byte(crypto.random_bytes(16), 1, 16)}
    -- Set version 4 (bits 4-7 of byte 7).
    b[7] = (b[7] % 16) + 64  -- 0100xxxx
    -- Set variant (bits 6-7 of byte 9).
    b[9] = (b[9] % 64) + 128 -- 10xxxxxx
    return string.format(
        "%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
        b[1],b[2],b[3],b[4], b[5],b[6], b[7],b[8], b[9],b[10],
        b[11],b[12],b[13],b[14],b[15],b[16])
end

-----------------------------------------------------------------------
-- HTTP helpers.
-----------------------------------------------------------------------

local function post_form(ua, endpoint, params)
    local headers = {
        ["User-Agent"] = ua,
    }
    local resp = http.post_form(endpoint, headers, params)
    if resp.status ~= 200 then
        error(endpoint .. ": HTTP " .. tostring(resp.status) .. ": " .. (resp.body or ""))
    end
    return resp.body
end

local function parse_json(body, context)
    local ok, result = pcall(json.decode, body)
    if not ok then
        error(context .. ": invalid JSON: " .. tostring(body):sub(1, 200))
    end
    return result
end

-----------------------------------------------------------------------
-- VK rate limit / error detection.
-----------------------------------------------------------------------

local vk_rate_limit_codes = {[6]=true, [9]=true, [14]=true, [29]=true}

local function check_vk_error(body_str, context)
    local data = parse_json(body_str, context)
    if data.error then
        local code = data.error.error_code or 0
        local msg = data.error.error_msg or ""
        if vk_rate_limit_codes[code] then
            error("rate_limit:" .. tostring(code) .. ":" .. msg)
        end
        error(context .. ": VK error " .. tostring(code) .. ": " .. msg)
    end
    return data
end

local function check_login_error(body_str, context)
    local data = parse_json(body_str, context)
    if data.error then
        local code = data.error_code or 0
        local desc = data.error_description or ""
        if code == 1105 or desc:find("Too many") then
            error("rate_limit:1105:" .. desc)
        end
        error(context .. ": login error: " .. (desc ~= "" and desc or data.error))
    end
    return data
end

-----------------------------------------------------------------------
-- Step 1: Get messages-scoped anonymous token.
-----------------------------------------------------------------------

local function vk_messages_token(ua)
    log.debug("auth: step1 — vkMessagesToken")
    local body = post_form(ua, vk_anon_token_url(), {
        client_id     = vk_client_id(),
        token_type    = "messages",
        client_secret = vk_client_secret(),
        version       = "1",
        app_id        = vk_client_id(),
    })

    local data = check_login_error(body, "vkMessagesToken")
    local token = data.data and data.data.access_token
    if not token or token == "" then
        error("vkMessagesToken: empty access_token in response: " .. body:sub(1, 200))
    end
    log.debug("auth: step1 done", "token_len", #token)
    return token
end

-----------------------------------------------------------------------
-- Step 2: Get join token (calls.getAnonymousToken) with captcha handling.
-----------------------------------------------------------------------

local function vk_join_token(ua, join_link, messages_token, display_name)
    log.debug("auth: step2 — vkJoinToken")
    local endpoint = vk_api_url() .. "/calls.getAnonymousToken?v=" .. vk_api_version()

    local params = {
        vk_join_link = "https://vk.com/call/join/" .. join_link,
        name         = display_name,
        access_token = messages_token,
    }

    local body = post_form(ua, endpoint, params)
    local data = parse_json(body, "vkJoinToken")

    -- Check for captcha (error 14).
    if data.error and data.error.error_code == 14 then
        if not captcha or not captcha.solve then
            error("rate_limit:14:" .. (data.error.error_msg or "Captcha needed"))
        end

        log.info("auth: captcha required, solving...", "sid", data.error.captcha_sid or "")

        local challenge = {
            redirect_uri = data.error.redirect_uri or "",
            captcha_sid  = data.error.captcha_sid or "",
            captcha_ts   = data.error.captcha_ts or 0,
            captcha_img  = data.error.captcha_img or "",
        }

        local result = captcha.solve(challenge)
        if not result or not result.success_token or result.success_token == "" then
            error("captcha solve failed: no success_token")
        end

        log.info("auth: captcha solved, retrying getAnonymousToken")

        -- Retry with captcha solution.
        params.success_token = result.success_token
        params.captcha_sid = challenge.captcha_sid

        -- Apply retry_params from solver (captcha_key, is_sound_captcha, captcha_ts, etc.).
        if result.retry_params then
            for k, v in pairs(result.retry_params) do
                params[k] = v
            end
        else
            -- Fallback formatting.
            local ts_str = string.format("%.3f", challenge.captcha_ts)
            ts_str = ts_str:gsub("0+$", ""):gsub("%.$", "")
            params.captcha_ts = ts_str
            params.captcha_attempt = "1"
        end

        body = post_form(ua, endpoint, params)
        data = parse_json(body, "vkJoinToken retry")
    end

    -- Check for other VK errors.
    if data.error then
        local code = data.error.error_code or 0
        local msg = data.error.error_msg or ""
        if vk_rate_limit_codes[code] then
            error("rate_limit:" .. tostring(code) .. ":" .. msg)
        end
        error("vkJoinToken: VK error " .. tostring(code) .. ": " .. msg)
    end

    local token = data.response and data.response.token
    if not token or token == "" then
        error("vkJoinToken: empty token in response: " .. body:sub(1, 200))
    end
    log.debug("auth: step2 done", "token_len", #token)
    return token
end

-----------------------------------------------------------------------
-- Step 3a: OK anonymous login.
-----------------------------------------------------------------------

local function ok_anon_login(ua, device_id)
    log.debug("auth: step3 — okAnonLogin")
    local session_data = json.encode({
        version        = 2,
        device_id      = device_id,
        client_version = 1.1,
        client_type    = "SDK_JS",
    })

    local body = post_form(ua, ok_api_url(), {
        session_data    = session_data,
        method          = "auth.anonymLogin",
        format          = "JSON",
        application_key = ok_app_key(),
    })

    local data = parse_json(body, "okAnonLogin")
    if not data.session_key or data.session_key == "" then
        error("okAnonLogin: empty session_key: " .. body:sub(1, 200))
    end
    log.debug("auth: step3 done")
    return data.session_key
end

-----------------------------------------------------------------------
-- Step 3b: OK auth with token (version 3, for authenticated flow).
-----------------------------------------------------------------------

local function ok_auth_with_token(ua, auth_token, device_id)
    log.debug("auth: ok_auth_with_token")
    local session_data = json.encode({
        version        = 3,
        device_id      = device_id,
        client_version = 1.1,
        client_type    = "SDK_JS",
        auth_token     = auth_token,
    })

    local body = post_form(ua, ok_api_url(), {
        session_data    = session_data,
        method          = "auth.anonymLogin",
        format          = "JSON",
        application_key = ok_app_key(),
    })

    local data = parse_json(body, "okAuthWithToken")
    if not data.session_key or data.session_key == "" then
        error("okAuthWithToken: empty session_key: " .. body:sub(1, 200))
    end
    return data.session_key
end

-----------------------------------------------------------------------
-- Step 4: Join conference → TURN credentials + WS endpoint.
-----------------------------------------------------------------------

local function ok_join_conference(ua, link, anonym_token, session_key)
    log.debug("auth: step4 — okJoinConference")
    local params = {
        joinLink        = link,
        isVideo         = "false",
        protocolVersion = "5",
        capabilities    = "2F7F",
        method          = "vchat.joinConversationByLink",
        format          = "JSON",
        application_key = ok_app_key(),
        session_key     = session_key,
    }
    if anonym_token and anonym_token ~= "" then
        params.anonymToken = anonym_token
    end

    local max_retries = 10
    local data
    for attempt = 0, max_retries do
        if attempt > 0 then
            log.warn("auth: no TURN URLs, retrying...", "attempt", attempt, "max", max_retries)
            local delay = math.min(attempt * 5000, 15000)
            time.sleep(delay)
        end

        local body = post_form(ua, ok_api_url(), params)
        data = parse_json(body, "okJoinConference")

        if data.error_code then
            error("okJoinConference: OK error " .. tostring(data.error_code) ..
                  ": " .. (data.error_msg or data.error_data or body:sub(1, 200)))
        end

        if data.turn_server and data.turn_server.urls and #data.turn_server.urls > 0 then
            break
        end

        if attempt == max_retries then
            error("okJoinConference: no TURN URLs after " .. max_retries .. " retries")
        end
    end

    log.info("auth: TURN server URLs", "count", #data.turn_server.urls)

    return {
        username    = data.turn_server.username,
        password    = data.turn_server.credential,
        urls        = data.turn_server.urls,
        ws_endpoint = data.endpoint,
        conv_id     = data.id,
        device_idx  = data.device_idx or 0,
    }
end

-----------------------------------------------------------------------
-- Authenticated flow: resolve VK/OK token → OK auth → join conference.
-----------------------------------------------------------------------

local function vk_get_call_token(ua, access_token)
    log.debug("auth: vkGetCallToken")
    local endpoint = vk_api_url_com() .. "/messages.getCallToken?v=" .. vk_api_version() .. "&client_id=" .. vk_client_id()

    local body = post_form(ua, endpoint, {
        env          = "production",
        access_token = access_token,
    })

    local data = parse_json(body, "vkGetCallToken")
    if data.error then
        local code = data.error.error_code or 0
        local msg = data.error.error_msg or ""
        if code == 5 then
            error("vkGetCallToken: VK token expired (error 5): " .. msg)
        end
        if vk_rate_limit_codes[code] then
            error("rate_limit:" .. tostring(code) .. ":" .. msg)
        end
        error("vkGetCallToken: VK error " .. tostring(code) .. ": " .. msg)
    end

    local token = data.response and data.response.token
    if not token or token == "" then
        error("vkGetCallToken: empty token: " .. body:sub(1, 200))
    end
    return token, data.response.api_base_url or ""
end

local function resolve_ok_auth_token(ua, token)
    -- Classify token by prefix.
    if token:sub(1, 6) == "vk1.a." then
        local auth_token, _ = vk_get_call_token(ua, token)
        return auth_token
    elseif token:sub(1, 1) == "$" then
        log.warn("auth: using OK auth_token directly — TTL unknown")
        return token
    else
        error("unrecognized token format (expected vk1.a.* or $*): " .. token:sub(1, 20) .. "...")
    end
end

local function auth_flow(join_link, display_name, token)
    local ua = random_user_agent()
    local device_id = generate_uuid()

    -- Step 1: Resolve token to OK auth_token.
    local auth_token = resolve_ok_auth_token(ua, token)

    -- Step 2: OK auth with token (version 3).
    local session_key = ok_auth_with_token(ua, auth_token, device_id)

    -- Step 3: Join conference without anonymToken.
    return ok_join_conference(ua, join_link, "", session_key)
end

-----------------------------------------------------------------------
-- Anonymous flow: messages token → join token → ok login → join conference.
-----------------------------------------------------------------------

local function anon_flow(join_link, display_name)
    local ua = random_user_agent()
    local device_id = generate_uuid()

    -- Steps 1+2 are sequential; step 3 (ok login) runs after them.
    -- (Lua is single-threaded, no goroutines — but the total time is fine.)

    -- Step 1: Get messages-scoped anonymous token.
    local messages_token = vk_messages_token(ua)

    -- Step 2: Get join token (with captcha handling).
    local join_token = vk_join_token(ua, join_link, messages_token, display_name)

    -- Step 3: OK anonymous login.
    local session_key = ok_anon_login(ua, device_id)

    -- Step 4: Join conference.
    return ok_join_conference(ua, join_link, join_token, session_key)
end

-----------------------------------------------------------------------
-- Main entry point.
-----------------------------------------------------------------------

function fetch_join_info(a)
    local join_link = a.join_link or ""
    local display_name = a.name or ""
    local token = a.token or ""

    if join_link == "" then
        error("auth: join_link is required")
    end

    if token ~= "" then
        log.info("auth: using authenticated flow", "token_prefix", token:sub(1, 6))
        return auth_flow(join_link, display_name, token)
    else
        log.info("auth: using anonymous flow")
        return anon_flow(join_link, display_name)
    end
end
