package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)

	switch cmd {
	case "keygen":
		cmdKeygen()
	case "sign":
		cmdSign()
	case "verify":
		cmdVerify()
	case "bundle":
		cmdBundle()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `scripts-sign — tool for managing scripts manifest signatures

Usage:
  scripts-sign keygen -out <dir>
  scripts-sign sign   -dir <scripts-dir> -priv <private-key-file> -base-url <url> [-version <v>] [-min-client <v>] [-apk <path>:<version>:<url>]
  scripts-sign verify -manifest <file> -pub <public-key-b64>
  scripts-sign bundle -src <scripts-dir> -dst <bundled-dir>`)
}

func cmdBundle() {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	src := fs.String("src", "", "source scripts directory")
	dst := fs.String("dst", "", "destination bundled directory (embed target)")
	_ = fs.Parse(os.Args[1:])
	if *src == "" || *dst == "" {
		die("-src and -dst required")
	}
	absSrc, _ := filepath.Abs(*src)
	absDst, _ := filepath.Abs(*dst)

	if err := os.MkdirAll(absDst, 0o755); err != nil {
		die("mkdir dst: %v", err)
	}
	existing, _ := os.ReadDir(absDst)
	for _, e := range existing {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(absDst, e.Name()))
	}

	scripts := map[string]map[string]any{}
	err := filepath.WalkDir(absSrc, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(absSrc, path)
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		scripts[rel] = map[string]any{
			"url":    "bundled://" + rel,
			"sha256": hex.EncodeToString(sum[:]),
			"size":   int64(len(data)),
		}
		if err := os.WriteFile(filepath.Join(absDst, rel), data, 0o644); err != nil {
			return err
		}
		return nil
	})
	mustOK(err, "walk src")

	mf := map[string]any{
		"version":      "bundled-" + time.Now().UTC().Format("2006.01.02"),
		"published_at": time.Now().UTC().Format(time.RFC3339),
		"scripts":      scripts,
		"signature":    "",
	}
	out, err := json.MarshalIndent(mf, "", "  ")
	mustOK(err, "marshal bundled manifest")
	mustOK(os.WriteFile(filepath.Join(absDst, "manifest.json"), append(out, '\n'), 0o644), "write bundled manifest")

	fmt.Printf("bundled %d files into %s\n", len(scripts), absDst)
}

func cmdKeygen() {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", ".", "directory to write keypair")
	_ = fs.Parse(os.Args[1:])

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	mustOK(err, "generate key")

	if err := os.MkdirAll(*out, 0o755); err != nil {
		die("mkdir: %v", err)
	}
	privPath := filepath.Join(*out, "scripts-signing.key")
	pubPath := filepath.Join(*out, "scripts-signing.pub")

	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	mustOK(os.WriteFile(privPath, []byte(privB64+"\n"), 0o600), "write priv")
	mustOK(os.WriteFile(pubPath, []byte(pubB64+"\n"), 0o644), "write pub")

	fmt.Printf("private key: %s\n", privPath)
	fmt.Printf("public key:  %s\n", pubPath)
	fmt.Printf("public (b64): %s\n", pubB64)
	fmt.Println("\nEmbed the public key via ldflags:")
	fmt.Printf("  -ldflags \"-X github.com/call-vpn/call-vpn/internal/scripts.DefaultPublicKey=%s\"\n", pubB64)
}

type apkFlag struct {
	path, version, url string
	sha256             string
}

func cmdSign() {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	dir := fs.String("dir", "", "directory with script files to publish")
	privPath := fs.String("priv", "", "path to Ed25519 private key (base64)")
	baseURL := fs.String("base-url", "", "base URL scripts will be served from (no trailing slash)")
	version := fs.String("version", "", "manifest version (default: timestamp)")
	minClient := fs.String("min-client", "", "optional minimum client version")
	apkSpec := fs.String("apk", "", "optional APK entry: path:version:url")
	_ = fs.Parse(os.Args[1:])

	if *dir == "" || *privPath == "" || *baseURL == "" {
		die("-dir, -priv, -base-url required")
	}
	*baseURL = strings.TrimRight(*baseURL, "/")

	privRaw, err := os.ReadFile(*privPath)
	mustOK(err, "read priv")
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(privRaw)))
	mustOK(err, "decode priv")
	if len(priv) != ed25519.PrivateKeySize {
		die("priv key size %d, want %d", len(priv), ed25519.PrivateKeySize)
	}

	scripts := map[string]map[string]any{}
	absDir, _ := filepath.Abs(*dir)

	err = filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(absDir, path)
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		scripts[rel] = map[string]any{
			"url":    *baseURL + "/" + rel,
			"sha256": hex.EncodeToString(sum[:]),
			"size":   int64(len(data)),
		}
		return nil
	})
	mustOK(err, "walk scripts dir")

	mf := map[string]any{
		"version":      resolveVersion(*version),
		"published_at": time.Now().UTC().Format(time.RFC3339),
		"scripts":      scripts,
	}
	if *minClient != "" {
		mf["min_client_version"] = *minClient
	}
	if *apkSpec != "" {
		apk, err := parseAPK(*apkSpec)
		mustOK(err, "parse apk")
		mf["apk"] = map[string]any{
			"version": apk.version,
			"url":     apk.url,
			"sha256":  apk.sha256,
		}
	} else {
		// Preserve existing APK entry from current manifest if present.
		existingManifest := filepath.Join(absDir, "manifest.json")
		if raw, err := os.ReadFile(existingManifest); err == nil {
			var existing map[string]any
			if json.Unmarshal(raw, &existing) == nil {
				if apk, ok := existing["apk"]; ok {
					mf["apk"] = apk
				}
			}
		}
	}

	payload := canonicalize(mf)
	sig := ed25519.Sign(ed25519.PrivateKey(priv), payload)
	mf["signature"] = base64.StdEncoding.EncodeToString(sig)

	out, err := json.MarshalIndent(mf, "", "  ")
	mustOK(err, "marshal manifest")
	outPath := filepath.Join(absDir, "manifest.json")
	mustOK(os.WriteFile(outPath, append(out, '\n'), 0o644), "write manifest")

	fmt.Printf("manifest: %s\n", outPath)
	fmt.Printf("version:  %s\n", mf["version"])
	fmt.Printf("scripts:  %d\n", len(scripts))
}

func cmdVerify() {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	mfPath := fs.String("manifest", "", "path to manifest.json")
	pub := fs.String("pub", "", "public key (base64)")
	_ = fs.Parse(os.Args[1:])

	if *mfPath == "" || *pub == "" {
		die("-manifest and -pub required")
	}
	raw, err := os.ReadFile(*mfPath)
	mustOK(err, "read manifest")
	pubKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*pub))
	mustOK(err, "decode pub")

	var obj map[string]json.RawMessage
	mustOK(json.Unmarshal(raw, &obj), "parse manifest")

	sigRaw, ok := obj["signature"]
	if !ok {
		die("manifest has no signature")
	}
	var sigB64 string
	mustOK(json.Unmarshal(sigRaw, &sigB64), "parse signature")
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	mustOK(err, "decode signature")

	delete(obj, "signature")
	payload := canonicalizeRaw(obj)

	if !ed25519.Verify(ed25519.PublicKey(pubKey), payload, sig) {
		die("signature INVALID")
	}
	fmt.Println("signature OK")
}

func canonicalize(m map[string]any) []byte {
	raw, _ := json.Marshal(m)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	delete(obj, "signature")
	return canonicalizeRaw(obj)
}

func canonicalizeRaw(obj map[string]json.RawMessage) []byte {
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
		// Перепарсить и переформатировать в компактный JSON
		var val any
		json.Unmarshal(obj[k], &val)
		vb, _ := json.Marshal(val)
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func resolveVersion(v string) string {
	if v != "" {
		return v
	}
	return time.Now().UTC().Format("2006.01.02-150405")
}

func parseAPK(spec string) (apkFlag, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) != 3 {
		return apkFlag{}, fmt.Errorf("apk spec must be path:version:url")
	}
	data, err := os.ReadFile(parts[0])
	if err != nil {
		return apkFlag{}, err
	}
	sum := sha256.Sum256(data)
	return apkFlag{
		path:    parts[0],
		version: parts[1],
		url:     parts[2],
		sha256:  hex.EncodeToString(sum[:]),
	}, nil
}

func mustOK(err error, what string) {
	if err != nil {
		die("%s: %v", what, err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "scripts-sign: "+format+"\n", args...)
	os.Exit(1)
}
