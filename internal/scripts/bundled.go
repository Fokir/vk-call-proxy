package scripts

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed bundled
var embeddedScripts embed.FS

func DefaultBundledFS() fs.FS {
	sub, err := fs.Sub(embeddedScripts, "bundled")
	if err != nil {
		return embeddedScripts
	}
	return sub
}

func LoadBundled(fsys fs.FS) (*Bundle, error) {
	if fsys == nil {
		return nil, errors.New("scripts: bundled fs is nil")
	}
	raw, err := fs.ReadFile(fsys, manifestFile)
	if err != nil {
		return nil, fmt.Errorf("scripts: bundled manifest: %w", err)
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("scripts: parse bundled manifest: %w", err)
	}
	files := make(map[string][]byte, len(mf.Scripts))
	for name := range mf.Scripts {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("scripts: read bundled %s: %w", name, err)
		}
		files[name] = data
	}
	return &Bundle{Manifest: &mf, Files: files, Source: SourceBundled}, nil
}
