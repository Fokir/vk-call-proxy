package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// RemoteSolver sends captcha challenges to an external captcha-service over HTTP.
type RemoteSolver struct {
	endpoint string
	client   *http.Client
}

// NewRemoteSolver creates a solver that talks to the captcha-service at the given endpoint.
func NewRemoteSolver(endpoint string) *RemoteSolver {
	return &RemoteSolver{
		endpoint: endpoint,
		client:   &http.Client{Timeout: 2 * time.Minute},
	}
}

type solveRequest struct {
	RedirectURI string `json:"redirect_uri"`
}

type solveResponse struct {
	SuccessToken string `json:"success_token,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *RemoteSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	body, err := json.Marshal(solveRequest{RedirectURI: ch.RedirectURI})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+"/solve", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("captcha-service request: %w", err)
	}
	defer resp.Body.Close()

	var result solveResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Error != "" {
		return nil, fmt.Errorf("captcha-service error: %s", result.Error)
	}
	if result.SuccessToken == "" {
		return nil, fmt.Errorf("captcha-service returned empty success_token")
	}

	return &provider.CaptchaResult{SuccessToken: result.SuccessToken}, nil
}
