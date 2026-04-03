package bind

import (
	"context"
	"fmt"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// CaptchaCallback is implemented by the mobile app (Java/Kotlin) to show
// a WebView with the VK captcha and return the success_token.
type CaptchaCallback interface {
	// ShowCaptcha opens a WebView with redirectURI and blocks until the user
	// solves the captcha. Returns the success_token or "" on failure/cancel.
	ShowCaptcha(redirectURI string) string
}

// callbackSolver adapts a CaptchaCallback into provider.CaptchaSolver.
type callbackSolver struct {
	cb CaptchaCallback
}

func (s *callbackSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	token := s.cb.ShowCaptcha(ch.RedirectURI)
	if token == "" {
		return nil, fmt.Errorf("captcha cancelled or failed")
	}
	return &provider.CaptchaResult{SuccessToken: token}, nil
}
