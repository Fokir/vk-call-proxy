package captcha

import (
	"sync/atomic"

	"github.com/call-vpn/call-vpn/internal/scripts"
)

func hotVKConfig() *scripts.VKConfig {
	if m := activeScripts.Load(); m != nil {
		return m.VKConfig()
	}
	return nil
}

// captchaCheckboxAnswer returns the hot-reloaded checkbox answer or the
// compiled-in default.
func captchaCheckboxAnswer(defaultAnswer string) string {
	if c := hotVKConfig(); c != nil && c.Captcha.CheckboxAnswer != "" {
		return c.Captcha.CheckboxAnswer
	}
	return defaultAnswer
}

// captchaDebugInfo returns the hot-reloaded debug_info fallback or the
// compiled-in default.
func captchaDebugInfo(defaultValue string) string {
	if c := hotVKConfig(); c != nil && c.Captcha.DebugInfoFallback != "" {
		return c.Captcha.DebugInfoFallback
	}
	return defaultValue
}

// captchaUserAgent returns the hot-reloaded User-Agent for DirectSolver or the
// compiled-in default.
func captchaUserAgent(defaultUA string) string {
	if c := hotVKConfig(); c != nil && c.Captcha.DirectSolver.UserAgent != "" {
		return c.Captcha.DirectSolver.UserAgent
	}
	return defaultUA
}

// captchaLanguage returns the hot-reloaded language for DirectSolver device info.
func captchaLanguage(defaultLang string) string {
	if c := hotVKConfig(); c != nil && c.Captcha.DirectSolver.Language != "" {
		return c.Captcha.DirectSolver.Language
	}
	return defaultLang
}

// captchaLanguages returns the hot-reloaded languages list for DirectSolver device info.
func captchaLanguages(defaultLangs []string) []string {
	if c := hotVKConfig(); c != nil && len(c.Captcha.DirectSolver.Languages) > 0 {
		return c.Captcha.DirectSolver.Languages
	}
	return defaultLangs
}

// captchaAPIVersion returns the hot-reloaded VK captcha API version or the
// compiled-in default.
func captchaAPIVersion(defaultVersion string) string {
	if c := hotVKConfig(); c != nil && c.Captcha.APIVersion != "" {
		return c.Captcha.APIVersion
	}
	return defaultVersion
}

var activeScripts atomic.Pointer[scripts.Manager]

// SetScriptsManager registers the hot-reload manager for captcha scripts.
// Call once at startup, after scripts.NewManager(...).Start(ctx). Safe to call
// with nil to detach.
func SetScriptsManager(m *scripts.Manager) {
	activeScripts.Store(m)
}

// scriptsFile returns the content of a script file from the active manager,
// falling back to the provided defaultContent if the manager is not set, has
// no current bundle, or the file is missing.
func scriptsFile(name, defaultContent string) string {
	m := activeScripts.Load()
	if m == nil {
		return defaultContent
	}
	if data, ok := m.File(name); ok {
		return string(data)
	}
	return defaultContent
}

// reportScriptFailure notifies the manager that a captcha solve failed while
// using hot-reloaded scripts. Triggers a force-check if the failure threshold
// is crossed.
func reportScriptFailure(scriptID string) {
	if m := activeScripts.Load(); m != nil {
		m.ReportFailure(scriptID)
	}
}
