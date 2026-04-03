package captcha

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// DirectSolver solves VK captcha by making direct API calls,
// mimicking the browser captchaNotRobot flow without actual browser.
type DirectSolver struct{}

func NewDirectSolver() *DirectSolver {
	return &DirectSolver{}
}

func (s *DirectSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	token, err := solveDirectAPI(ctx, ch.RedirectURI)
	if err != nil {
		return nil, err
	}
	return &provider.CaptchaResult{SuccessToken: token}, nil
}

func solveDirectAPI(ctx context.Context, redirectURI string) (string, error) {
	// Parse session_token from redirect_uri.
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("parse redirect_uri: %w", err)
	}
	sessionToken := u.Query().Get("session_token")
	if sessionToken == "" {
		return "", fmt.Errorf("no session_token in redirect_uri")
	}
	domain := u.Query().Get("domain")
	if domain == "" {
		domain = "vk.com"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	adFp := randomAdFp(r)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// Step 1: captchaNotRobot.settings
	slog.Debug("captcha direct: settings")
	settingsResp, err := vkCaptchaPost(ctx, client, ua, "captchaNotRobot.settings", url.Values{
		"session_token": {sessionToken},
		"domain":        {domain},
		"adFp":          {adFp},
		"access_token":  {""},
	})
	if err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	slog.Debug("captcha direct: settings response", "resp", string(settingsResp))

	// Wait a bit, mimicking sensor collection delay.
	select {
	case <-time.After(time.Duration(500+r.Intn(500)) * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Step 2: captchaNotRobot.componentDone
	slog.Debug("captcha direct: componentDone")
	device := generateDeviceInfo(r)
	deviceJSON, _ := json.Marshal(device)
	browserFp := generateBrowserFp(r)

	_, err = vkCaptchaPost(ctx, client, ua, "captchaNotRobot.componentDone", url.Values{
		"session_token": {sessionToken},
		"domain":        {domain},
		"adFp":          {adFp},
		"browser_fp":    {""},
		"device":        {string(deviceJSON)},
		"access_token":  {""},
	})
	if err != nil {
		return "", fmt.Errorf("componentDone: %w", err)
	}

	// Simulate sensor data collection period (1-2 seconds).
	sensorsDelay := time.Duration(1000+r.Intn(1000)) * time.Millisecond
	select {
	case <-time.After(sensorsDelay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Step 3: captchaNotRobot.check
	slog.Debug("captcha direct: check")
	cursor := generateCursor(r)
	cursorJSON, _ := json.Marshal(cursor)
	rtt := generateConnectionRtt(r)
	rttJSON, _ := json.Marshal(rtt)
	downlink := generateConnectionDownlink(r)
	downlinkJSON, _ := json.Marshal(downlink)

	// Proof-of-work hash — find SHA-256 with leading zeros.
	hash, debugInfo := computeProofOfWork(sessionToken, r)

	checkResp, err := vkCaptchaPost(ctx, client, ua, "captchaNotRobot.check", url.Values{
		"session_token":      {sessionToken},
		"domain":             {domain},
		"adFp":               {adFp},
		"accelerometer":      {"[]"},
		"gyroscope":          {"[]"},
		"motion":             {"[]"},
		"cursor":             {string(cursorJSON)},
		"taps":               {"[]"},
		"connectionRtt":      {string(rttJSON)},
		"connectionDownlink": {string(downlinkJSON)},
		"browser_fp":         {browserFp},
		"hash":               {hash},
		"answer":             {"e30="}, // base64("{}")
		"debug_info":         {debugInfo},
		"access_token":       {""},
	})
	if err != nil {
		return "", fmt.Errorf("check: %w", err)
	}

	// Parse success_token from response.
	var checkResult struct {
		Response struct {
			Status       string `json:"status"`
			SuccessToken string `json:"success_token"`
			ShowCaptcha  string `json:"show_captcha_type"`
		} `json:"response"`
		Error *struct {
			Code int    `json:"error_code"`
			Msg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(checkResp, &checkResult); err != nil {
		return "", fmt.Errorf("parse check response: %w (%s)", err, string(checkResp))
	}
	if checkResult.Error != nil {
		return "", fmt.Errorf("check error %d: %s", checkResult.Error.Code, checkResult.Error.Msg)
	}
	if checkResult.Response.SuccessToken == "" {
		if checkResult.Response.ShowCaptcha != "" {
			return "", fmt.Errorf("VK wants visual captcha (type=%s), direct solve not possible", checkResult.Response.ShowCaptcha)
		}
		return "", fmt.Errorf("no success_token in check response: %s", string(checkResp))
	}

	// Step 4: endSession (best effort).
	slog.Debug("captcha direct: endSession")
	vkCaptchaPost(ctx, client, ua, "captchaNotRobot.endSession", url.Values{
		"session_token": {sessionToken},
		"domain":        {domain},
		"adFp":          {adFp},
		"access_token":  {""},
	})

	return checkResult.Response.SuccessToken, nil
}

func vkCaptchaPost(ctx context.Context, client *http.Client, ua, method string, data url.Values) ([]byte, error) {
	endpoint := fmt.Sprintf("https://api.vk.ru/method/%s?v=5.275", method)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Origin", "https://id.vk.com")
	req.Header.Set("Referer", "https://id.vk.com/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return body, nil
}

type deviceInfo struct {
	ScreenWidth           int    `json:"screenWidth"`
	ScreenHeight          int    `json:"screenHeight"`
	ScreenAvailWidth      int    `json:"screenAvailWidth"`
	ScreenAvailHeight     int    `json:"screenAvailHeight"`
	InnerWidth            int    `json:"innerWidth"`
	InnerHeight           int    `json:"innerHeight"`
	DevicePixelRatio      int    `json:"devicePixelRatio"`
	Language              string `json:"language"`
	Languages             []string `json:"languages"`
	Webdriver             bool   `json:"webdriver"`
	HardwareConcurrency   int    `json:"hardwareConcurrency"`
	DeviceMemory          int    `json:"deviceMemory"`
	ConnectionEffType     string `json:"connectionEffectiveType"`
	NotificationsPermission string `json:"notificationsPermission"`
}

func generateDeviceInfo(r *rand.Rand) deviceInfo {
	widths := []int{1920, 2560, 1680, 1440}
	heights := []int{1080, 1440, 1050, 900}
	idx := r.Intn(len(widths))
	w, h := widths[idx], heights[idx]
	return deviceInfo{
		ScreenWidth:           w,
		ScreenHeight:          h,
		ScreenAvailWidth:      w,
		ScreenAvailHeight:     h - 48,
		InnerWidth:            w/2 + r.Intn(200),
		InnerHeight:           h - 100 - r.Intn(100),
		DevicePixelRatio:      1,
		Language:              "ru",
		Languages:             []string{"ru"},
		Webdriver:             false,
		HardwareConcurrency:   []int{8, 12, 16, 24}[r.Intn(4)],
		DeviceMemory:          []int{8, 16, 32}[r.Intn(3)],
		ConnectionEffType:     "4g",
		NotificationsPermission: "denied",
	}
}

type cursorPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func generateCursor(r *rand.Rand) []cursorPoint {
	n := 8 + r.Intn(8)
	points := make([]cursorPoint, n)
	x := 600 + r.Intn(400)
	y := 300 + r.Intn(200)
	for i := range points {
		x += r.Intn(60) - 30
		y += r.Intn(40) - 20
		if x < 0 { x = 50 }
		if y < 0 { y = 50 }
		points[i] = cursorPoint{X: x, Y: y}
	}
	return points
}

func generateConnectionRtt(r *rand.Rand) []int {
	base := 50 + r.Intn(100)
	rtt := make([]int, 11)
	for i := range rtt {
		rtt[i] = base
	}
	return rtt
}

func generateConnectionDownlink(r *rand.Rand) []float64 {
	base := 5.0 + float64(r.Intn(15))
	dl := make([]float64, 11)
	for i := range dl {
		dl[i] = base
	}
	return dl
}

func generateBrowserFp(r *rand.Rand) string {
	data := make([]byte, 32)
	r.Read(data)
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}

func randomAdFp(r *rand.Rand) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
	b := make([]byte, 21)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// computeProofOfWork finds a SHA-256 hash with 3 leading hex zeros (12-bit difficulty).
// Based on HAR observation: hash starts with "0003..." (3 leading zero hex digits).
func computeProofOfWork(sessionToken string, r *rand.Rand) (string, string) {
	prefix := sessionToken
	if len(prefix) > 64 {
		prefix = prefix[:64]
	}

	for i := 0; i < 10_000_000; i++ {
		nonce := fmt.Sprintf("%s:%d", prefix, i)
		h := sha256.Sum256([]byte(nonce))
		hexStr := hex.EncodeToString(h[:])
		// Check for 3 leading hex zeros (like "000...")
		if hexStr[:3] == "000" {
			debugHash := sha256.Sum256([]byte(fmt.Sprintf("%d", i)))
			return hexStr, hex.EncodeToString(debugHash[:])
		}
	}

	// Fallback — unlikely to reach.
	h := sha256.Sum256([]byte(prefix))
	return hex.EncodeToString(h[:]), hex.EncodeToString(h[:])
}
