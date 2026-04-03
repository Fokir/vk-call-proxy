package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// ChromedpSolver solves VK captchas using a headless Chrome browser.
// Only one browser instance runs at a time (guarded by mutex).
type ChromedpSolver struct {
	mu sync.Mutex
}

// NewChromedpSolver creates a new ChromedpSolver.
func NewChromedpSolver() *ChromedpSolver {
	return &ChromedpSolver{}
}

// SolveCaptcha implements provider.CaptchaSolver.
func (s *ChromedpSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := SolveCaptchaURI(ctx, ch.RedirectURI)
	if err != nil {
		return nil, err
	}
	return &provider.CaptchaResult{SuccessToken: token}, nil
}

// SolveCaptchaURI opens redirectURI in headless Chrome, simulates mouse
// movement, and intercepts the captchaNotRobot.check XHR response to
// extract the success_token. It is exported so that external callers
// (e.g. captcha-service) can use it directly with their own concurrency.
func SolveCaptchaURI(ctx context.Context, redirectURI string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.NoSandbox,
		chromedp.WindowSize(1280, 800),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	tokenCh := make(chan string, 1)

	// Listen for network responses containing the captcha check result.
	chromedp.ListenTarget(taskCtx, func(ev any) {
		resp, ok := ev.(*network.EventResponseReceived)
		if !ok {
			return
		}
		if resp.Response == nil || !strings.Contains(resp.Response.URL, "captchaNotRobot.check") {
			return
		}
		reqID := resp.RequestID
		// Read body in a goroutine to avoid blocking the event loop.
		go func() {
			body, err := network.GetResponseBody(reqID).Do(taskCtx)
			if err != nil {
				return
			}
			var result struct {
				Response struct {
					SuccessToken string `json:"success_token"`
				} `json:"response"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return
			}
			if result.Response.SuccessToken != "" {
				select {
				case tokenCh <- result.Response.SuccessToken:
				default:
				}
			}
		}()
	})

	// Run browser actions: enable network events, navigate, wait, move mouse.
	if err := chromedp.Run(taskCtx,
		network.Enable(),
		chromedp.Navigate(redirectURI),
		chromedp.WaitReady("body"),
		simulateMouse(),
	); err != nil {
		return "", fmt.Errorf("chromedp run: %w", err)
	}

	// Wait for the success token or context cancellation.
	select {
	case token := <-tokenCh:
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// simulateMouse returns a chromedp action that generates realistic mouse
// movement events along a roughly straight line with random jitter.
func simulateMouse() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))

		// Random start point in the center area of the viewport.
		x0 := 400.0 + r.Float64()*200.0
		y0 := 300.0 + r.Float64()*100.0

		// Random end point offset.
		dx := 200.0 + r.Float64()*200.0 // 200-400px right
		dy := 30.0 + r.Float64()*30.0   // 30-60px down
		x1 := x0 + dx
		y1 := y0 + dy

		steps := 10 + r.Intn(6) // 10-15 intermediate points

		for i := 0; i <= steps; i++ {
			t := float64(i) / float64(steps)
			x := x0 + t*(x1-x0) + (r.Float64()*30.0 - 15.0) // jitter +/-15px horizontal
			y := y0 + t*(y1-y0) + (r.Float64()*10.0 - 5.0)   // jitter +/-5px vertical

			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
				return fmt.Errorf("mouse move step %d: %w", i, err)
			}

			// Random delay between 50-200ms.
			delay := time.Duration(50+r.Intn(151)) * time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
}
