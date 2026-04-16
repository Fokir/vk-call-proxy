package scripts

import "encoding/json"

type VKConfig struct {
	VK      VKSection      `json:"vk"`
	Captcha CaptchaSection `json:"captcha"`
}

type VKSection struct {
	APIVersion string   `json:"api_version"`
	WS         WSParams `json:"ws"`
	UserAgents []string `json:"user_agents"`
}

type WSParams struct {
	AppVersion   string `json:"app_version"`
	Version      string `json:"version"`
	Capabilities string `json:"capabilities"`
	Compression  string `json:"compression"`
}

type CaptchaSection struct {
	APIVersion        string                `json:"api_version"`
	CheckboxAnswer    string                `json:"checkbox_answer"`
	DebugInfoFallback string                `json:"debug_info_fallback"`
	DirectSolver      DirectSolverConfig    `json:"direct_solver"`
	ChromedpSolver    ChromedpSolverConfig  `json:"chromedp_solver"`
	Stealth           StealthConfig         `json:"stealth"`
}

type DirectSolverConfig struct {
	UserAgent string   `json:"user_agent"`
	Language  string   `json:"language"`
	Languages []string `json:"languages"`
}

type ChromedpSolverConfig struct {
	UserAgent         string   `json:"user_agent"`
	CheckboxSelectors []string `json:"checkbox_selectors"`
}

type StealthConfig struct {
	Languages     []string `json:"languages"`
	Platform      string   `json:"platform"`
	WebGLVendor   string   `json:"webgl_vendor"`
	WebGLRenderer string   `json:"webgl_renderer"`
}

// ParseVKConfig parses raw JSON into VKConfig. Returns error only on malformed
// JSON; missing fields yield zero values that callers should treat as "use
// built-in default".
func ParseVKConfig(data []byte) (*VKConfig, error) {
	var cfg VKConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// VKConfig reads vk-config.json from the current bundle and parses it.
// Returns nil if the file is missing or unparseable.
func (m *Manager) VKConfig() *VKConfig {
	if m == nil {
		return nil
	}
	data, ok := m.File("vk-config.json")
	if !ok {
		return nil
	}
	cfg, err := ParseVKConfig(data)
	if err != nil {
		return nil
	}
	return cfg
}
