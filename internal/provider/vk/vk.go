// Package vk implements provider.Service for VK Calls.
// It fetches TURN credentials via the VK/OK authentication chain
// and connects to VK WebSocket signaling for relay-to-relay mode.
package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sync"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

const (
	vkClientID     = "6287487"
	vkClientSecret = "QbYic1K3lEV5kTGiqlq2"
	vkAPIVersion   = "5.275"
	okAppKey       = "CGMMEJLGDIHBABABA"
	httpTimeout    = 20 * time.Second
)

// classifyToken detects token format by prefix.
// Returns "vk" for VK access tokens (vk1.a.*), "ok" for OK auth tokens ($*).
func classifyToken(token string) string {
	if strings.HasPrefix(token, "vk1.a.") {
		return "vk"
	}
	if strings.HasPrefix(token, "$") {
		return "ok"
	}
	return "unknown"
}

// Service implements provider.Service for VK Calls.
type Service struct {
	callLink string
	captcha  provider.CaptchaSolver
}

// Option configures a VK Service.
type Option func(*Service)

// WithCaptchaSolver sets a captcha solver for automatic captcha resolution.
func WithCaptchaSolver(s provider.CaptchaSolver) Option {
	return func(svc *Service) { svc.captcha = s }
}

// Compile-time checks.
var _ provider.Service = (*Service)(nil)
var _ provider.TokenAuthProvider = (*Service)(nil)

// NewService creates a VK call service provider.
func NewService(callLink string, opts ...Option) *Service {
	s := &Service{callLink: callLink}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Service) Name() string { return "vk" }

// FetchCredentials obtains anonymous TURN credentials from VK using the
// 4-step authentication chain. Each call generates a fresh anonymous identity.
func (s *Service) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	ji, err := s.FetchJoinInfo(ctx)
	if err != nil {
		return nil, err
	}
	creds := ji.Credentials
	return &creds, nil
}

// FetchJoinInfo performs the VK authentication chain and returns
// TURN credentials, WebSocket endpoint, and conversation info.
//
// Flow (4 steps, OK login runs in parallel with steps 1-2):
//  1. get_anonym_token(token_type=messages) → messages token
//  2. calls.getAnonymousToken(messages_token) → join token
//  3. auth.anonymLogin → session_key (parallel)
//  4. vchat.joinConversationByLink(join_token, session_key) → TURN + WS
func (s *Service) FetchJoinInfo(ctx context.Context) (*provider.JoinInfo, error) {
	ua := randomUserAgent()

	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	// Step 3: OK anonymous login — independent of steps 1-2, run in parallel.
	var sessionKey string
	var errOK error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deviceID := uuid.New().String()
		sessionKey, errOK = okAnonLogin(ctx, client, ua, deviceID)
	}()

	// Step 1: Get messages-scoped anonymous token (no payload/scopes step needed)
	messagesToken, err := vkMessagesToken(ctx, client, ua)
	if err != nil {
		wg.Wait()
		return nil, fmt.Errorf("step1 messages token: %w", err)
	}

	// Step 2: Get join token (call-specific)
	joinToken, err := s.vkJoinToken(ctx, client, ua, s.callLink, messagesToken)
	if err != nil {
		wg.Wait()
		return nil, fmt.Errorf("step2 join token: %w", err)
	}

	// Wait for step 3 to complete.
	wg.Wait()
	if errOK != nil {
		return nil, fmt.Errorf("step3 ok login: %w", errOK)
	}

	// Step 4: Join conference and extract TURN credentials + WS info
	result, err := okJoinConference(ctx, client, ua, s.callLink, joinToken, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("step4 join conference: %w", err)
	}

	return &provider.JoinInfo{
		Credentials: *result.creds,
		WSEndpoint:  result.endpoint,
		ConvID:      result.convID,
		DeviceIdx:   result.deviceIdx,
	}, nil
}

// resolveOKAuthToken converts a token to an OK auth_token.
// For "vk" tokens: calls messages.getCallToken API.
// For "ok" tokens: returns as-is with a TTL warning.
func resolveOKAuthToken(ctx context.Context, client *http.Client, ua string, token string) (authToken string, err error) {
	switch classifyToken(token) {
	case "vk":
		authToken, _, err = vkGetCallToken(ctx, client, ua, token)
		return authToken, err
	case "ok":
		slog.Warn("using OK auth_token directly — TTL unknown, token may expire without warning")
		return token, nil
	default:
		return "", fmt.Errorf("unrecognized token format (expected vk1.a.* or $*): %.20s...", token)
	}
}

// FetchJoinInfoWithToken implements provider.TokenAuthProvider.
// Uses the authorized 3-step flow: resolve OK auth_token → auth.anonymLogin(v3) → joinConference.
func (s *Service) FetchJoinInfoWithToken(ctx context.Context, token string) (*provider.JoinInfo, error) {
	ua := randomUserAgent()
	client := &http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	// Step 1: Resolve token to OK auth_token
	authToken, err := resolveOKAuthToken(ctx, client, ua, token)
	if err != nil {
		return nil, fmt.Errorf("resolve auth token: %w", err)
	}

	// Step 2: OK auth with token (version 3)
	deviceID := uuid.New().String()
	sessionKey, err := okAuthWithToken(ctx, client, ua, authToken, deviceID)
	if err != nil {
		return nil, fmt.Errorf("ok auth with token: %w", err)
	}

	// Step 3: Join conference without anonymToken (empty string = omit)
	result, err := okJoinConference(ctx, client, ua, s.callLink, "", sessionKey)
	if err != nil {
		return nil, fmt.Errorf("join conference: %w", err)
	}

	return &provider.JoinInfo{
		Credentials: *result.creds,
		WSEndpoint:  result.endpoint,
		ConvID:      result.convID,
		DeviceIdx:   result.deviceIdx,
	}, nil
}

// ConnectSignaling connects to VK WebSocket signaling.
func (s *Service) ConnectSignaling(ctx context.Context, info *provider.JoinInfo, logger *slog.Logger) (provider.SignalingClient, error) {
	return ConnectSignaling(ctx, info.WSEndpoint, info.DeviceIdx, logger)
}

// --- VK auth chain (private) ---

func vkMessagesToken(ctx context.Context, client *http.Client, ua string) (string, error) {
	data := url.Values{
		"client_id":     {vkClientID},
		"token_type":    {"messages"},
		"client_secret": {vkClientSecret},
		"version":       {"1"},
		"app_id":        {vkClientID},
	}
	body, err := httpPost(ctx, client, ua, "https://login.vk.com/?act=get_anonym_token", data)
	if err != nil {
		return "", err
	}
	if rle := checkLoginRateLimit(body); rle != nil {
		slog.Warn("VK login rate limit", "code", rle.Code, "msg", rle.Message)
		return "", rle
	}
	var resp struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if resp.Data.AccessToken == "" {
		return "", fmt.Errorf("empty token in response: %s", string(body))
	}
	return resp.Data.AccessToken, nil
}

func (s *Service) vkJoinToken(ctx context.Context, client *http.Client, ua string, link, token3 string) (string, error) {
	data := url.Values{
		"vk_join_link": {fmt.Sprintf("https://vk.com/call/join/%s", link)},
		"name":         {provider.RandomDisplayName()},
		"access_token": {token3},
	}
	endpoint := fmt.Sprintf("https://api.vk.ru/method/calls.getAnonymousToken?v=%s", vkAPIVersion)

	body, err := httpPost(ctx, client, ua, endpoint, data)
	if err != nil {
		return "", err
	}

	// Check for captcha (error 14) before generic rate limit check.
	var errResp vkErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != nil && errResp.Error.Code == 14 {
		if s.captcha == nil {
			slog.Warn("VK API captcha required, no solver configured", "endpoint", endpoint)
			return "", &provider.RateLimitError{Code: 14, Message: errResp.Error.Msg}
		}

		slog.Info("captcha required, solving...", "captcha_sid", errResp.Error.CaptchaSID)
		start := time.Now()

		result, solveErr := s.captcha.SolveCaptcha(ctx, &provider.CaptchaChallenge{
			RedirectURI: errResp.Error.RedirectURI,
			CaptchaSID:  errResp.Error.CaptchaSID,
			CaptchaTS:   errResp.Error.CaptchaTS,
			CaptchaImg:  errResp.Error.CaptchaImg,
		})
		if solveErr != nil {
			slog.Warn("captcha solve failed", "err", solveErr, "duration", time.Since(start))
			return "", fmt.Errorf("captcha solve: %w", solveErr)
		}
		slog.Info("captcha solved", "duration", time.Since(start))

		// Retry with captcha solution.
		data.Set("success_token", result.SuccessToken)
		data.Set("captcha_sid", errResp.Error.CaptchaSID)
		data.Set("captcha_ts", fmt.Sprintf("%.2f", errResp.Error.CaptchaTS))
		data.Set("captcha_attempt", "1")

		body, err = vkAPIPost(ctx, client, ua, endpoint, data)
		if err != nil {
			return "", fmt.Errorf("retry after captcha: %w", err)
		}
	} else if rle := checkVKRateLimit(body); rle != nil {
		slog.Warn("VK API rate limit", "code", rle.Code, "msg", rle.Message, "endpoint", endpoint)
		return "", rle
	}

	var resp struct {
		Response struct {
			Token string `json:"token"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if resp.Response.Token == "" {
		return "", fmt.Errorf("empty token in response: %s", string(body))
	}
	return resp.Response.Token, nil
}

func okAnonLogin(ctx context.Context, client *http.Client, ua string, deviceID string) (string, error) {
	sessionData := fmt.Sprintf(`{"version":2,"device_id":"%s","client_version":1.1,"client_type":"SDK_JS"}`, deviceID)
	data := url.Values{
		"session_data":    {sessionData},
		"method":          {"auth.anonymLogin"},
		"format":          {"JSON"},
		"application_key": {okAppKey},
	}
	body, err := httpPost(ctx, client, ua, "https://calls.okcdn.ru/fb.do", data)
	if err != nil {
		return "", err
	}
	var resp struct {
		SessionKey string `json:"session_key"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if resp.SessionKey == "" {
		return "", fmt.Errorf("empty session_key in response: %s", string(body))
	}
	return resp.SessionKey, nil
}

// vkGetCallToken exchanges a VK access_token for an OK auth_token via messages.getCallToken.
func vkGetCallToken(ctx context.Context, client *http.Client, ua string, accessToken string) (authToken string, apiBaseURL string, err error) {
	data := url.Values{
		"env":          {"production"},
		"access_token": {accessToken},
	}
	endpoint := fmt.Sprintf("https://api.vk.com/method/messages.getCallToken?v=%s&client_id=%s", vkAPIVersion, vkClientID)
	body, err := httpPost(ctx, client, ua, endpoint, data)
	if err != nil {
		return "", "", err
	}
	var errResp vkErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != nil {
		if errResp.Error.Code == 5 {
			return "", "", fmt.Errorf("VK token expired (error 5): %s", errResp.Error.Msg)
		}
		if rle := checkVKRateLimit(body); rle != nil {
			return "", "", rle
		}
		return "", "", fmt.Errorf("VK API error %d: %s", errResp.Error.Code, errResp.Error.Msg)
	}
	var resp struct {
		Response struct {
			Token      string `json:"token"`
			APIBaseURL string `json:"api_base_url"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", fmt.Errorf("parse getCallToken response: %w", err)
	}
	if resp.Response.Token == "" {
		return "", "", fmt.Errorf("empty token in getCallToken response: %s", string(body))
	}
	return resp.Response.Token, resp.Response.APIBaseURL, nil
}

// okAuthWithToken performs OK auth.anonymLogin with an auth_token (version 3).
func okAuthWithToken(ctx context.Context, client *http.Client, ua string, authToken string, deviceID string) (string, error) {
	sessionData := fmt.Sprintf(`{"version":3,"device_id":"%s","client_version":1.1,"client_type":"SDK_JS","auth_token":"%s"}`, deviceID, authToken)
	data := url.Values{
		"session_data":    {sessionData},
		"method":          {"auth.anonymLogin"},
		"format":          {"JSON"},
		"application_key": {okAppKey},
	}
	body, err := httpPost(ctx, client, ua, "https://calls.okcdn.ru/fb.do", data)
	if err != nil {
		return "", err
	}
	var resp struct {
		SessionKey string `json:"session_key"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse auth response: %w", err)
	}
	if resp.SessionKey == "" {
		return "", fmt.Errorf("empty session_key in auth response: %s", string(body))
	}
	return resp.SessionKey, nil
}

type joinConferenceResult struct {
	creds     *provider.Credentials
	endpoint  string
	convID    string
	deviceIdx int
}

func okJoinConference(ctx context.Context, client *http.Client, ua string, link, anonymToken, sessionKey string) (*joinConferenceResult, error) {
	data := url.Values{
		"joinLink":        {link},
		"isVideo":         {"false"},
		"protocolVersion": {"5"},
		"capabilities":    {"2F7F"},
		"method":          {"vchat.joinConversationByLink"},
		"format":          {"JSON"},
		"application_key": {okAppKey},
		"session_key":     {sessionKey},
	}
	if anonymToken != "" {
		data.Set("anonymToken", anonymToken)
	}
	body, err := httpPost(ctx, client, ua, "https://calls.okcdn.ru/fb.do", data)
	if err != nil {
		return nil, err
	}
	var resp struct {
		TurnServer struct {
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
			URLs       []string `json:"urls"`
		} `json:"turn_server"`
		Endpoint  string `json:"endpoint"`
		ID        string `json:"id"`
		DeviceIdx int    `json:"device_idx"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(resp.TurnServer.URLs) == 0 {
		const maxRetries = 10
		for attempt := 1; attempt <= maxRetries; attempt++ {
			slog.Warn("no TURN URLs in response, retrying...", "attempt", attempt, "max", maxRetries)
			delay := min(time.Duration(attempt)*5*time.Second, 15*time.Second)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("no TURN URLs in response (cancelled after %d retries): %s", attempt-1, string(body))
			case <-time.After(delay):
			}
			body, err = httpPost(ctx, client, ua, "https://calls.okcdn.ru/fb.do", data)
			if err != nil {
				return nil, err
			}
			resp = struct {
				TurnServer struct {
					Username   string   `json:"username"`
					Credential string   `json:"credential"`
					URLs       []string `json:"urls"`
				} `json:"turn_server"`
				Endpoint  string `json:"endpoint"`
				ID        string `json:"id"`
				DeviceIdx int    `json:"device_idx"`
			}{}
			if err := json.Unmarshal(body, &resp); err != nil {
				return nil, fmt.Errorf("parse response: %w", err)
			}
			if len(resp.TurnServer.URLs) > 0 {
				break
			}
		}
		if len(resp.TurnServer.URLs) == 0 {
			return nil, fmt.Errorf("no TURN URLs in response after %d retries: %s", maxRetries, string(body))
		}
	}

	slog.Info("TURN server URLs from VK", "urls", resp.TurnServer.URLs, "count", len(resp.TurnServer.URLs))
	host, port := turn.ParseTURNURL(resp.TurnServer.URLs[0])
	var servers []provider.TURNServer
	for _, u := range resp.TurnServer.URLs {
		h, p := turn.ParseTURNURL(u)
		servers = append(servers, provider.TURNServer{Host: h, Port: p})
	}
	return &joinConferenceResult{
		creds: &provider.Credentials{
			Username: resp.TurnServer.Username,
			Password: resp.TurnServer.Credential,
			Host:     host,
			Port:     port,
			Servers:  servers,
		},
		endpoint:  resp.Endpoint,
		convID:    resp.ID,
		deviceIdx: resp.DeviceIdx,
	}, nil
}

// vkErrorResponse represents a VK API JSON error envelope.
type vkErrorResponse struct {
	Error *struct {
		Code        int     `json:"error_code"`
		Msg         string  `json:"error_msg"`
		RedirectURI string  `json:"redirect_uri"`
		CaptchaSID  string  `json:"captcha_sid"`
		CaptchaTS   float64 `json:"captcha_ts"`
		CaptchaImg  string  `json:"captcha_img"`
	} `json:"error"`
}

// vkRateLimitCodes are VK API error codes that indicate rate limiting.
var vkRateLimitCodes = map[int]bool{
	6:  true, // Too many requests per second
	9:  true, // Flood control
	14: true, // Captcha needed
	29: true, // Rate limit reached
}

// checkVKRateLimit parses the response body for VK API error codes.
func checkVKRateLimit(body []byte) *provider.RateLimitError {
	var resp vkErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
		return nil
	}
	if vkRateLimitCodes[resp.Error.Code] {
		return &provider.RateLimitError{
			Code:    resp.Error.Code,
			Message: resp.Error.Msg,
		}
	}
	return nil
}

// checkLoginRateLimit parses login.vk.com response for auth flood errors.
func checkLoginRateLimit(body []byte) *provider.RateLimitError {
	var resp struct {
		Error     string `json:"error"`
		ErrorCode int    `json:"error_code"`
		ErrorDesc string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.ErrorCode == 1105 || strings.Contains(resp.ErrorDesc, "Too many") {
		return &provider.RateLimitError{
			Code:    1105,
			Message: resp.ErrorDesc,
		}
	}
	return nil
}

// vkAPIPost performs an HTTP POST to a VK API endpoint and checks
// the response for rate limit errors before returning.
func vkAPIPost(ctx context.Context, client *http.Client, ua string, endpoint string, data url.Values) ([]byte, error) {
	body, err := httpPost(ctx, client, ua, endpoint, data)
	if err != nil {
		return nil, err
	}
	if rle := checkVKRateLimit(body); rle != nil {
		slog.Warn("VK API rate limit", "code", rle.Code, "msg", rle.Message, "endpoint", endpoint)
		return nil, rle
	}
	return body, nil
}

func httpPost(ctx context.Context, client *http.Client, ua string, endpoint string, data url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
