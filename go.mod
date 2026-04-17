module github.com/call-vpn/call-vpn

go 1.25.7

require (
	github.com/cbeuw/connutil v1.0.1
	github.com/chromedp/cdproto v0.0.0-20250403032234-65de8f5d025b
	github.com/chromedp/chromedp v0.13.7
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/pion/dtls/v3 v3.1.2
	github.com/pion/logging v0.2.4
	github.com/pion/turn/v4 v4.1.4
	github.com/pion/webrtc/v4 v4.2.9
	github.com/yuin/gopher-lua v1.1.2
	gvisor.dev/gvisor v0.0.0-20260122175437-89a5d21be8f0
)

require (
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20250211171154-1ae217ad3535 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/pion/datachannel v1.6.0 // indirect
	github.com/pion/ice/v4 v4.2.1 // indirect
	github.com/pion/interceptor v0.1.44 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.1 // indirect
	github.com/pion/sctp v1.9.2 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.10 // indirect
	github.com/pion/stun/v3 v3.1.1 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/wlynxg/anet v0.0.6-0.20250109065809-5501d401a269 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/exp v0.0.0-20231110203233-9a3e6036ecaa // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)

replace github.com/pion/dtls/v3 => github.com/Fokir/dtls/v3 v3.1.2-browser
