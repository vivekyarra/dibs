package intake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Credentials is what `dibs intake register` saves.
type Credentials struct {
	Server  string `json:"server"`
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
	Name    string `json:"name"`
}

// DefaultCredsPath is ~/.dibs/intake.json.
func DefaultCredsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".dibs", "intake.json")
	}
	return filepath.Join(home, ".dibs", "intake.json")
}

// SaveCredentials writes creds atomically with mode 0600.
func SaveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ") // #nosec G117 -- the credentials file exists to hold the key; written 0600
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".intake-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	_ = tmp.Chmod(0o600)     // best effort; not supported on every platform (final Chmod below)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// LoadCredentials reads and validates a credentials file.
func LoadCredentials(path string) (Credentials, error) {
	var c Credentials
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read credentials (run `dibs intake register` first): %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse credentials %s: %w", path, err)
	}
	if c.Server == "" || c.AgentID == "" || c.APIKey == "" {
		return c, fmt.Errorf("credentials %s missing server, agent_id or api_key", path)
	}
	return c, nil
}

// MaskKey shows only enough of a secret to identify it.
func MaskKey(k string) string {
	if len(k) <= 12 {
		return "****"
	}
	return k[:4] + "…" + k[len(k)-4:]
}
