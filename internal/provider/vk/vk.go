// Package vk implements provider.Service for VK Calls.
// It fetches TURN credentials via the 6-step VK/OK authentication chain
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
	vkAPIVersion   = "5.264"
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
}

// Compile-time check.
var _ provider.Service = (*Service)(nil)

// NewService creates a VK call service provider.
func NewService(callLink string) *Service {
	return &Service{callLink: callLink}
}

func (s *Service) Name() string { return "vk" }

// FetchCredentials obtains anonymous TURN credentials from VK using the
// 6-step authentication chain. Each call generates a fresh anonymous identity.
func (s *Service) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	ji, err := s.FetchJoinInfo(ctx)
	if err != nil {
		return nil, err
	}
	creds := ji.Credentials
	return &creds, nil
}

// FetchJoinInfo performs the full 6-step VK authentication chain and returns
// TURN credentials, WebSocket endpoint, and conversation info.
func (s *Service) FetchJoinInfo(ctx context.Context) (*provider.JoinInfo, error) {
	// Select a random User-Agent once per session so all requests look consistent.
	ua := randomUserAgent()

	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	// Step 1: Get anonymous token
	token1, err := vkAnonToken(ctx, client, ua)
	if err != nil {
		return nil, fmt.Errorf("step1 anon token: %w", err)
	}

	// Step 5: OK anonymous login — independent of steps 2-4, run in parallel.
	var token5 string
	var err5 error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deviceID := uuid.New().String()
		token5, err5 = okAnonLogin(ctx, client, ua, deviceID)
	}()

	// Steps 2-3-4: sequential chain (each depends on previous).
	token2, err := vkAnonPayload(ctx, client, ua, token1)
	if err != nil {
		wg.Wait()
		return nil, fmt.Errorf("step2 anon payload: %w", err)
	}

	token3, err := vkMessagesToken(ctx, client, ua, token2)
	if err != nil {
		wg.Wait()
		return nil, fmt.Errorf("step3 messages token: %w", err)
	}

	token4, err := vkJoinToken(ctx, client, ua, s.callLink, token3)
	if err != nil {
		wg.Wait()
		return nil, fmt.Errorf("step4 join token: %w", err)
	}

	// Wait for step 5 to complete.
	wg.Wait()
	if err5 != nil {
		return nil, fmt.Errorf("step5 ok login: %w", err5)
	}

	// Step 6: Join conference and extract TURN credentials + WS info
	result, err := okJoinConference(ctx, client, ua, s.callLink, token4, token5)
	if err != nil {
		return nil, fmt.Errorf("step6 join conference: %w", err)
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
	return ConnectSignaling(ctx, info.WSEndpoint, logger)
}

// --- VK auth chain (private) ---

func vkAnonToken(ctx context.Context, client *http.Client, ua string) (string, error) {
	data := url.Values{
		"client_secret":           {vkClientSecret},
		"client_id":               {vkClientID},
		"scopes":                  {"audio_anonymous,video_anonymous,photos_anonymous,profile_anonymous"},
		"isApiOauthAnonymEnabled": {"false"},
		"version":                 {"1"},
		"app_id":                  {vkClientID},
	}
	body, err := httpPost(ctx, client, ua, "https://login.vk.ru/?act=get_anonym_token", data)
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

func vkAnonPayload(ctx context.Context, client *http.Client, ua string, token1 string) (string, error) {
	endpoint := fmt.Sprintf("https://api.vk.ru/method/calls.getAnonymousAccessTokenPayload?v=%s&client_id=%s", vkAPIVersion, vkClientID)
	data := url.Values{"access_token": {token1}}
	body, err := vkAPIPost(ctx, client, ua, endpoint, data)
	if err != nil {
		return "", err
	}
	var resp struct {
		Response struct {
			Payload string `json:"payload"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if resp.Response.Payload == "" {
		return "", fmt.Errorf("empty payload in response: %s", string(body))
	}
	return resp.Response.Payload, nil
}

func vkMessagesToken(ctx context.Context, client *http.Client, ua string, token2 string) (string, error) {
	data := url.Values{
		"client_id":     {vkClientID},
		"token_type":    {"messages"},
		"payload":       {token2},
		"client_secret": {vkClientSecret},
		"version":       {"1"},
		"app_id":        {vkClientID},
	}
	body, err := httpPost(ctx, client, ua, "https://login.vk.ru/?act=get_anonym_token", data)
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

func vkJoinToken(ctx context.Context, client *http.Client, ua string, link, token3 string) (string, error) {
	data := url.Values{
		"vk_join_link": {fmt.Sprintf("https://vk.com/call/join/%s", link)},
		"name":         {provider.RandomDisplayName()},
		"access_token": {token3},
	}
	endpoint := fmt.Sprintf("https://api.vk.ru/method/calls.getAnonymousToken?v=%s", vkAPIVersion)
	body, err := vkAPIPost(ctx, client, ua, endpoint, data)
	if err != nil {
		return "", err
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
		return nil, fmt.Errorf("no TURN URLs in response: %s", string(body))
	}

	host, port := turn.ParseTURNURL(resp.TurnServer.URLs[0])
	return &joinConferenceResult{
		creds: &provider.Credentials{
			Username: resp.TurnServer.Username,
			Password: resp.TurnServer.Credential,
			Host:     host,
			Port:     port,
		},
		endpoint:  resp.Endpoint,
		convID:    resp.ID,
		deviceIdx: resp.DeviceIdx,
	}, nil
}

// vkErrorResponse represents a VK API JSON error envelope.
type vkErrorResponse struct {
	Error *struct {
		Code int    `json:"error_code"`
		Msg  string `json:"error_msg"`
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

// checkLoginRateLimit parses login.vk.ru response for auth flood errors.
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
