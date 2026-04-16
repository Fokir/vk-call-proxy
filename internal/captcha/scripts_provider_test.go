//go:build !android

package captcha

import "testing"

func TestScriptsFile_FallbackWhenUnset(t *testing.T) {
	SetScriptsManager(nil)
	defer SetScriptsManager(nil)

	got := scriptsFile("stealth.js", "DEFAULT")
	if got != "DEFAULT" {
		t.Fatalf("expected fallback, got %q", got)
	}
}

func TestStealthJSConstIsNonEmpty(t *testing.T) {
	if len(stealthJS) < 100 {
		t.Fatalf("stealthJS fallback is too short: %d bytes", len(stealthJS))
	}
}
