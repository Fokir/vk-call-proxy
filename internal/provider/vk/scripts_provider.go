package vk

import (
	"sync/atomic"

	"github.com/call-vpn/call-vpn/internal/scripts"
)

var activeScripts atomic.Pointer[scripts.Manager]

// SetScriptsManager registers the hot-reload manager for VK-specific config.
// Call once at startup after scripts.NewManager(...).Start(ctx).
func SetScriptsManager(m *scripts.Manager) {
	activeScripts.Store(m)
}

func hotVKConfig() *scripts.VKConfig {
	if m := activeScripts.Load(); m != nil {
		return m.VKConfig()
	}
	return nil
}

// apiVersion returns the hot-reloaded VK API version or the compiled-in default.
func apiVersion() string {
	if c := hotVKConfig(); c != nil && c.VK.APIVersion != "" {
		return c.VK.APIVersion
	}
	return vkAPIVersion
}

// userAgents returns the hot-reloaded user-agent pool or the compiled-in default.
func userAgents() []string {
	if c := hotVKConfig(); c != nil && len(c.VK.UserAgents) > 0 {
		return c.VK.UserAgents
	}
	return userAgentPool
}

// wsAppVersion, wsVersion, wsCapabilities, wsCompression return WS query
// params — hot-reloaded values if present, otherwise compiled-in defaults.
func wsAppVersion() string {
	if c := hotVKConfig(); c != nil && c.VK.WS.AppVersion != "" {
		return c.VK.WS.AppVersion
	}
	return "1.1"
}

func wsVersion() string {
	if c := hotVKConfig(); c != nil && c.VK.WS.Version != "" {
		return c.VK.WS.Version
	}
	return "5"
}

func wsCapabilities() string {
	if c := hotVKConfig(); c != nil && c.VK.WS.Capabilities != "" {
		return c.VK.WS.Capabilities
	}
	return "2F7F"
}

func wsCompression() string {
	if c := hotVKConfig(); c != nil && c.VK.WS.Compression != "" {
		return c.VK.WS.Compression
	}
	return "deflate-raw"
}

// reportFailure notifies the manager that a VK operation failed while using
// hot-reloaded config. Triggers a force-check if the failure threshold is crossed.
func reportFailure(scriptID string) {
	if m := activeScripts.Load(); m != nil {
		m.ReportFailure(scriptID)
	}
}
