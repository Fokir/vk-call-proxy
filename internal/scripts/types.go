package scripts

import "time"

type Manifest struct {
	Version          string                  `json:"version"`
	PublishedAt      time.Time               `json:"published_at"`
	MinClientVersion string                  `json:"min_client_version,omitempty"`
	Scripts          map[string]ScriptEntry  `json:"scripts"`
	APK              *APKEntry               `json:"apk,omitempty"`
	Signature        string                  `json:"signature"`
}

type ScriptEntry struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

type APKEntry struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size,omitempty"`
}

type Bundle struct {
	Manifest *Manifest
	Files    map[string][]byte
	Source   BundleSource
}

type BundleSource int

const (
	SourceBundled BundleSource = iota
	SourceLocal
	SourceRemote
)

func (s BundleSource) String() string {
	switch s {
	case SourceBundled:
		return "bundled"
	case SourceLocal:
		return "local"
	case SourceRemote:
		return "remote"
	default:
		return "unknown"
	}
}

func (b *Bundle) File(name string) ([]byte, bool) {
	if b == nil {
		return nil, false
	}
	data, ok := b.Files[name]
	return data, ok
}
