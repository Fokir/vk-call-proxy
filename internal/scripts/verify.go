package scripts

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrBadSignature = errors.New("scripts: manifest signature invalid")
	ErrBadHash      = errors.New("scripts: file sha256 mismatch")
	ErrNoPublicKey  = errors.New("scripts: public key not configured")
)

func verifyManifest(raw []byte, publicKeyB64 string) (*Manifest, error) {
	if publicKeyB64 == "" {
		return nil, ErrNoPublicKey
	}
	pubKey, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return nil, fmt.Errorf("scripts: decode public key: %w", err)
	}
	if len(pubKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("scripts: public key size %d, want %d", len(pubKey), ed25519.PublicKeySize)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("scripts: parse manifest: %w", err)
	}
	if m.Signature == "" {
		return nil, ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return nil, fmt.Errorf("scripts: decode signature: %w", err)
	}

	payload, err := canonicalManifestPayload(raw)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(ed25519.PublicKey(pubKey), payload, sig) {
		return nil, ErrBadSignature
	}
	return &m, nil
}

func canonicalManifestPayload(raw []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("scripts: canonicalize manifest: %w", err)
	}
	delete(obj, "signature")

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortStrings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		var val any
		if err := json.Unmarshal(obj[k], &val); err != nil {
			return nil, fmt.Errorf("scripts: re-parse %s: %w", k, err)
		}
		vb, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("scripts: re-marshal %s: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func verifyScript(data []byte, expectedHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != expectedHex {
		return fmt.Errorf("%w: got %s, want %s", ErrBadHash, got, expectedHex)
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// compareVersions compares semver-ish strings ("0.24.1" vs "0.25.0"). Returns
// -1, 0, or 1. Non-numeric parts compare as 0.
func compareVersions(a, b string) int {
	ap, bp := parseVersion(a), parseVersion(b)
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(ap) {
			x = ap[i]
		}
		if i < len(bp) {
			y = bp[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func parseVersion(v string) []int {
	parts := []int{}
	current := 0
	hasDigit := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c >= '0' && c <= '9' {
			current = current*10 + int(c-'0')
			hasDigit = true
		} else if c == '.' {
			if hasDigit {
				parts = append(parts, current)
			} else {
				parts = append(parts, 0)
			}
			current = 0
			hasDigit = false
		} else {
			break
		}
	}
	if hasDigit {
		parts = append(parts, current)
	}
	return parts
}
