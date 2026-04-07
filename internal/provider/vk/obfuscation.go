package vk

import (
	"fmt"
	"math/rand/v2"
	"net/url"
)

// joinParamTemplate is the WebSocket query parameter string VK expects.
// Placeholders: %d = deviceIdx, %s = URL-encoded user agent.
const joinParamTemplate = "platform=WEB&appVersion=1.1&version=5&device=browser&capabilities=2F7F&clientType=VK&tgt=join&compression=deflate-raw&deviceIdx=%d&ua=%s"

var userAgentPool = []string{
	// Chrome 135 — Windows 10/11
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36 Edg/134.0.0.0",
	// Chrome 135 — macOS
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_7_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	// Chrome 134 — Windows (предыдущая стабильная)
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36",
	// Firefox 136 — Windows
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:136.0) Gecko/20100101 Firefox/136.0",
	// Chrome 135 — Linux
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
	// Edge 135 — Windows
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0",
}

func joinParamsWithDeviceIdx(deviceIdx int) string {
	ua := randomUserAgent()
	return fmt.Sprintf(joinParamTemplate, deviceIdx, url.QueryEscape(ua))
}

func randomUserAgent() string {
	return userAgentPool[rand.IntN(len(userAgentPool))]
}
