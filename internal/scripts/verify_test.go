package scripts

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"sort"
	"testing"
)

func signManifestForTest(t *testing.T, mf map[string]any, priv ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := json.Marshal(mf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("parse: %v", err)
	}
	delete(obj, "signature")

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

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
		_ = json.Unmarshal(obj[k], &val)
		vb, _ := json.Marshal(val)
		buf.Write(vb)
	}
	buf.WriteByte('}')

	sig := ed25519.Sign(priv, buf.Bytes())
	mf["signature"] = base64.StdEncoding.EncodeToString(sig)
	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatalf("marshal signed: %v", err)
	}
	return out
}

func TestVerifyManifest_SignedWithIndentation(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	mf := map[string]any{
		"version":      "1",
		"published_at": "2026-04-15T10:00:00Z",
		"scripts": map[string]any{
			"stealth.js": map[string]any{
				"url":    "https://example.com/stealth.js",
				"sha256": "deadbeef",
				"size":   42,
			},
		},
	}
	signed := signManifestForTest(t, mf, priv)

	pubB64 := base64.StdEncoding.EncodeToString(pub)
	got, err := verifyManifest(signed, pubB64)
	if err != nil {
		t.Fatalf("verifyManifest: %v", err)
	}
	if got.Version != "1" {
		t.Fatalf("version: %q", got.Version)
	}
	if len(got.Scripts) != 1 {
		t.Fatalf("scripts: %d", len(got.Scripts))
	}
}

func TestVerifyManifest_TamperedFails(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	mf := map[string]any{
		"version":      "1",
		"published_at": "2026-04-15T10:00:00Z",
		"scripts":      map[string]any{},
	}
	signed := signManifestForTest(t, mf, priv)
	tampered := bytes.Replace(signed, []byte(`"1"`), []byte(`"2"`), 1)

	pubB64 := base64.StdEncoding.EncodeToString(pub)
	if _, err := verifyManifest(tampered, pubB64); err == nil {
		t.Fatal("expected verify failure on tampered manifest")
	}
}
