package ghrunner

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// settingsDir is where tabrunner stores its runner identity, mirroring the real
// runner's working directory (.runner/.credentials live alongside the binary).
const settingsDir = ".tabrunner"

func resolveDir(dir string) string {
	if dir == "" {
		return settingsDir
	}
	return dir
}

// runnerState is the in-memory pairing of persisted settings and the
// reconstructed private key.
type runnerState struct {
	Settings Settings
	Key      *rsa.PrivateKey
}

// saveState writes the runner identity and key material to dir.
func saveState(dir string, s Settings, key *rsa.PrivateKey) error {
	dir = resolveDir(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "settings.json"), s); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "credentials_rsaparams.json"), exportParams(key))
}

// loadState reads settings + key material and reconstructs the private key.
func loadState(dir string) (*runnerState, error) {
	dir = resolveDir(dir)
	var s Settings
	if err := readJSON(filepath.Join(dir, "settings.json"), &s); err != nil {
		return nil, err
	}
	var p rsaParams
	if err := readJSON(filepath.Join(dir, "credentials_rsaparams.json"), &p); err != nil {
		return nil, err
	}
	key, err := p.toKey()
	if err != nil {
		return nil, fmt.Errorf("reconstruct key: %w", err)
	}
	return &runnerState{Settings: s, Key: key}, nil
}

// StateExists reports whether a registered runner identity is present in dir.
func StateExists(dir string) bool {
	_, err := os.Stat(filepath.Join(resolveDir(dir), "settings.json"))
	return err == nil
}

// LoadSettings returns the persisted runner settings (without the key material).
func LoadSettings(dir string) (*Settings, error) {
	dir = resolveDir(dir)
	var s Settings
	if err := readJSON(filepath.Join(dir, "settings.json"), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
