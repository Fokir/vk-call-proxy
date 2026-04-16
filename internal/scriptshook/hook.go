// Package scriptshook wires a scripts.Manager into the captcha and VK
// packages so that their package-level hot-reload hooks are populated in one
// place. Each binary (cmd/server, cmd/server-ui, cmd/captcha-service,
// mobile/bind) calls Register once after starting the manager.
package scriptshook

import (
	"github.com/call-vpn/call-vpn/internal/captcha"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/scripts"
)

// Register connects the given manager to all subsystems that consume hot
// config/scripts. Safe to call with nil to detach.
func Register(m *scripts.Manager) {
	captcha.SetScriptsManager(m)
	vk.SetScriptsManager(m)
}
