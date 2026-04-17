package captcha

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// ChainSolver tries multiple CaptchaSolvers in order.
// Each solver is attempted; if it fails, the next one is tried.
// Returns the first successful result, or the last error if all fail.
type ChainSolver struct {
	solvers []provider.CaptchaSolver
}

// NewChainSolver creates a ChainSolver that tries solvers in order.
func NewChainSolver(solvers ...provider.CaptchaSolver) *ChainSolver {
	return &ChainSolver{solvers: solvers}
}

func (c *ChainSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	if len(c.solvers) == 0 {
		return nil, fmt.Errorf("no captcha solvers configured")
	}

	var lastErr error
	for i, s := range c.solvers {
		result, err := s.SolveCaptcha(ctx, ch)
		if err == nil {
			return result, nil
		}
		lastErr = err
		slog.Info("captcha solver failed, trying next",
			"solver", fmt.Sprintf("%T", s),
			"index", i,
			"err", err,
		)
		// A failed solver may have burned the current captcha_sid.
		// Refresh the challenge so the next solver gets a fresh one.
		if ch.RefreshFunc != nil && i < len(c.solvers)-1 {
			fresh, rerr := ch.RefreshFunc()
			if rerr != nil {
				slog.Warn("captcha refresh failed", "err", rerr)
			} else {
				*ch = *fresh
				slog.Info("captcha refreshed", "new_sid", ch.CaptchaSID)
			}
		}
	}
	return nil, fmt.Errorf("all captcha solvers failed: %w", lastErr)
}
