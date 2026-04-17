package provider

import "context"

// CaptchaChallenge contains data from a VK API error_code 14 response.
type CaptchaChallenge struct {
	RedirectURI string  // iframe URL for captchaNotRobot flow
	CaptchaSID  string  // captcha_sid from error
	CaptchaTS   float64 // captcha_ts from error
	CaptchaImg  string  // fallback: classic captcha image URL

	// RefreshFunc, if set, is called between solver attempts to obtain a fresh
	// captcha challenge (new captcha_sid) after a solver burns the current one.
	RefreshFunc func() (*CaptchaChallenge, error)
}

// CaptchaResult contains the solution to a captcha challenge.
type CaptchaResult struct {
	SuccessToken string // from captchaNotRobot.check (priority)
	CaptchaKey   string // from classic image captcha (fallback)
}

// CaptchaSolver solves VK captcha challenges.
// Implementations: RemoteSolver (HTTP), BrowserSolver (system browser), CallbackSolver (mobile).
type CaptchaSolver interface {
	SolveCaptcha(ctx context.Context, ch *CaptchaChallenge) (*CaptchaResult, error)
}
