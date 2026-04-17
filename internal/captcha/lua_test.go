package captcha

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

func TestLuaSolver_SimpleReturn(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			return "test-token-123"
		end
	`))

	res, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		CaptchaSID: "sid1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SuccessToken != "test-token-123" {
		t.Fatalf("got token %q, want %q", res.SuccessToken, "test-token-123")
	}
}

func TestLuaSolver_ScriptError(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			error("something went wrong")
		end
	`))

	_, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Fatalf("error should contain script message, got: %v", err)
	}
}

func TestLuaSolver_NoScript(t *testing.T) {
	s := NewLuaSolver(nil) // no manager, no override

	_, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no solver.lua") {
		t.Fatalf("error should mention missing script, got: %v", err)
	}
}

func TestLuaSolver_Timeout(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			while true do end
		end
	`))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := s.SolveCaptcha(ctx, &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("error should mention context, got: %v", err)
	}
}

func TestLuaSolver_ChallengeFields(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			if challenge.redirect_uri ~= "https://example.com/captcha" then
				error("bad redirect_uri: " .. tostring(challenge.redirect_uri))
			end
			if challenge.captcha_sid ~= "sid-42" then
				error("bad captcha_sid: " .. tostring(challenge.captcha_sid))
			end
			if challenge.captcha_ts ~= 1234567890.5 then
				error("bad captcha_ts: " .. tostring(challenge.captcha_ts))
			end
			return "fields-ok"
		end
	`))

	res, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		RedirectURI: "https://example.com/captcha",
		CaptchaSID:  "sid-42",
		CaptchaTS:   1234567890.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SuccessToken != "fields-ok" {
		t.Fatalf("got token %q, want %q", res.SuccessToken, "fields-ok")
	}
}
