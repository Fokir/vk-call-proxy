package captcha

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// ErrNotSlider is returned when the captcha is not a slider type.
// ChainSolver uses this to fall back to the next solver.
var ErrNotSlider = fmt.Errorf("captcha is not slider type")

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
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

	// Step 0: Fetch the captcha page to extract captcha_settings and PoW params.
	pageData, err := fetchCaptchaPage(ctx, client, ua, redirectURI)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotSlider, err)
	}

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

	// Step 2: captchaNotRobot.getContent (slider puzzle).
	slog.Debug("captcha direct: getContent (slider)")
	contentResp, err := vkCaptchaPost(ctx, client, ua, "captchaNotRobot.getContent", url.Values{
		"session_token":    {sessionToken},
		"domain":           {domain},
		"adFp":             {adFp},
		"captcha_settings": {pageData.sliderSettings},
		"access_token":     {""},
	})
	if err != nil {
		return "", fmt.Errorf("getContent: %w", err)
	}

	puzzle, err := parseSliderContent(contentResp)
	if err != nil {
		return "", fmt.Errorf("parse slider content: %w", err)
	}

	answer, err := solveSlider(puzzle)
	if err != nil {
		return "", fmt.Errorf("solve slider: %w", err)
	}
	sliderAnswer := encodeSliderAnswer(answer)
	slog.Debug("captcha direct: slider solved", "position", len(answer)/2, "maxPos", len(puzzle.swapPairs)/2)

	// Simulate delay (sensor collection + user solving).
	delay := time.Duration(1500+r.Intn(2000)) * time.Millisecond
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Step 3: captchaNotRobot.componentDone
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

	// Simulate slider interaction delay.
	sliderDelay := time.Duration(500+r.Intn(1000)) * time.Millisecond
	select {
	case <-time.After(sliderDelay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Step 4: captchaNotRobot.check
	slog.Debug("captcha direct: check")
	cursor := generateSliderCursor(r)
	cursorJSON, _ := json.Marshal(cursor)
	rtt := generateConnectionRtt(r)
	rttJSON, _ := json.Marshal(rtt)
	downlink := generateConnectionDownlink(r)
	downlinkJSON, _ := json.Marshal(downlink)

	// Proof-of-work hash (SHA-256 with leading zeros, computed from HTML page params).
	hash := computeProofOfWork(pageData.powInput, pageData.powDifficulty)

	// debug_info is a hardcoded constant in VK's JS.
	const debugInfo = "1d3e9babfd3a74f4588bf90cf5c30d3e8e89a0e2a4544da8de8bbf4d78a32f5c"

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
		"answer":             {sliderAnswer},
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
			return "", fmt.Errorf("captcha check failed (type=%s, status=%s)", checkResult.Response.ShowCaptcha, checkResult.Response.Status)
		}
		return "", fmt.Errorf("no success_token in check response: %s", string(checkResp))
	}

	// Step 5: endSession (best effort).
	slog.Debug("captcha direct: endSession")
	vkCaptchaPost(ctx, client, ua, "captchaNotRobot.endSession", url.Values{
		"session_token": {sessionToken},
		"domain":        {domain},
		"adFp":          {adFp},
		"access_token":  {""},
	})

	return checkResult.Response.SuccessToken, nil
}

// captchaPageData holds data extracted from the captcha HTML page.
type captchaPageData struct {
	sliderSettings string // captcha_settings for slider type
	powInput       string // proof-of-work input string
	powDifficulty  int    // number of leading hex zeros required
}

// fetchCaptchaPage fetches the captcha HTML page and extracts
// slider settings and proof-of-work parameters.
func fetchCaptchaPage(ctx context.Context, client *http.Client, ua, redirectURI string) (*captchaPageData, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", redirectURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	data := &captchaPageData{powDifficulty: 2} // default difficulty

	// Extract slider captcha_settings: {"type":"slider","settings":"<value>"}
	reSettings := regexp.MustCompile(`"type"\s*:\s*"slider"\s*,\s*"settings"\s*:\s*"([^"]+)"`)
	if match := reSettings.FindSubmatch(body); match != nil {
		data.sliderSettings = strings.ReplaceAll(string(match[1]), `\/`, `/`)
	} else {
		return nil, fmt.Errorf("slider captcha_settings not found in captcha page")
	}

	// Extract PoW input: const powInput = "...";
	rePow := regexp.MustCompile(`powInput\s*=\s*"([^"]+)"`)
	if match := rePow.FindSubmatch(body); match != nil {
		data.powInput = string(match[1])
	}

	// Extract PoW difficulty: const difficulty = N;
	reDiff := regexp.MustCompile(`difficulty\s*=\s*(\d+)`)
	if match := reDiff.FindSubmatch(body); match != nil {
		if d, err := strconv.Atoi(string(match[1])); err == nil {
			data.powDifficulty = d
		}
	}

	return data, nil
}

// generateSliderCursor generates realistic cursor movement for slider interaction.
// Simulates: mouse enters → moves to slider → drags right → releases.
func generateSliderCursor(r *rand.Rand) []cursorPoint {
	n := 20 + r.Intn(30)
	points := make([]cursorPoint, 0, n)

	// Start near the slider area.
	x := 500 + r.Intn(200)
	y := 400 + r.Intn(200)
	points = append(points, cursorPoint{X: x, Y: y})

	// Move towards slider handle.
	targetX := 580 + r.Intn(40)
	targetY := 830 + r.Intn(20)
	steps := 5 + r.Intn(5)
	for i := 1; i <= steps; i++ {
		px := x + (targetX-x)*i/steps + r.Intn(6) - 3
		py := y + (targetY-y)*i/steps + r.Intn(6) - 3
		points = append(points, cursorPoint{X: px, Y: py})
	}

	// Drag slider right (increasing x).
	dragSteps := 10 + r.Intn(15)
	cx, cy := targetX, targetY
	for i := 0; i < dragSteps; i++ {
		cx += 5 + r.Intn(15)
		cy += r.Intn(4) - 2
		points = append(points, cursorPoint{X: cx, Y: cy})
	}

	// Final position (hold).
	for i := 0; i < 2+r.Intn(3); i++ {
		points = append(points, cursorPoint{X: cx + r.Intn(2), Y: cy + r.Intn(2)})
	}

	return points
}

func vkCaptchaPost(ctx context.Context, client *http.Client, ua, method string, data url.Values) ([]byte, error) {
	endpoint := fmt.Sprintf("https://api.vk.com/method/%s?v=5.131", method)
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

// computeProofOfWork computes SHA-256(powInput + nonce) until the hash
// starts with `difficulty` leading hex zeros. Matches VK's JS implementation:
//   hash = SHA-256(powInput + nonceString)
//   while (!hash.startsWith("0".repeat(difficulty)))
func computeProofOfWork(powInput string, difficulty int) string {
	if powInput == "" || difficulty <= 0 {
		return ""
	}
	prefix := strings.Repeat("0", difficulty)
	for nonce := 1; nonce < 100_000_000; nonce++ {
		data := powInput + strconv.Itoa(nonce)
		h := sha256.Sum256([]byte(data))
		hexStr := hex.EncodeToString(h[:])
		if strings.HasPrefix(hexStr, prefix) {
			return hexStr
		}
	}
	return ""
}
