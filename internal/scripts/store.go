package scripts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	currentDirName  = "current"
	previousDirName = "previous"
	stagingDirName  = "staging"
	manifestFile    = "manifest.json"
)

type Store struct {
	root string
	log  Logger
}

func NewStore(root string, log Logger) *Store {
	if log == nil {
		log = noopLogger{}
	}
	return &Store{root: root, log: log}
}

func (s *Store) Root() string { return s.root }

func (s *Store) currentDir() string  { return filepath.Join(s.root, currentDirName) }
func (s *Store) previousDir() string { return filepath.Join(s.root, previousDirName) }
func (s *Store) stagingDir() string  { return filepath.Join(s.root, stagingDirName) }

func (s *Store) ensureRoot() error {
	if s.root == "" {
		return errors.New("scripts: empty store root")
	}
	return os.MkdirAll(s.root, 0o755)
}

func (s *Store) LoadCurrent() (*Bundle, error) {
	return s.loadFromDir(s.currentDir(), SourceLocal)
}

func (s *Store) loadFromDir(dir string, source BundleSource) (*Bundle, error) {
	mfPath := filepath.Join(dir, manifestFile)
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		return nil, err
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("scripts: parse stored manifest: %w", err)
	}
	files := make(map[string][]byte, len(mf.Scripts))
	for name := range mf.Scripts {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("scripts: read %s: %w", name, err)
		}
		files[name] = data
	}
	return &Bundle{Manifest: &mf, Files: files, Source: source}, nil
}

func (s *Store) Swap(bundle *Bundle, rawManifest []byte) error {
	if err := s.ensureRoot(); err != nil {
		return err
	}
	staging := s.stagingDir()
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}

	for name, data := range bundle.Files {
		dst := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(dst, data, 0o644); err != nil {
			return err
		}
	}
	mfPath := filepath.Join(staging, manifestFile)
	if err := writeFileAtomic(mfPath, rawManifest, 0o644); err != nil {
		return err
	}

	current := s.currentDir()
	prev := s.previousDir()

	if _, err := os.Stat(current); err == nil {
		_ = os.RemoveAll(prev)
		if err := os.Rename(current, prev); err != nil {
			return fmt.Errorf("scripts: rotate current->previous: %w", err)
		}
	}
	if err := os.Rename(staging, current); err != nil {
		return fmt.Errorf("scripts: promote staging->current: %w", err)
	}
	return nil
}

func (s *Store) Rollback() (*Bundle, error) {
	prev := s.previousDir()
	if _, err := os.Stat(prev); err != nil {
		return nil, fmt.Errorf("scripts: no previous bundle: %w", err)
	}
	current := s.currentDir()
	tmp := current + ".rollback-tmp"
	_ = os.RemoveAll(tmp)

	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, tmp); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(prev, current); err != nil {
		_ = os.Rename(tmp, current)
		return nil, err
	}
	if _, err := os.Stat(tmp); err == nil {
		_ = os.RemoveAll(tmp)
	}
	return s.LoadCurrent()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if _, err := os.Stat(name); err == nil {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
