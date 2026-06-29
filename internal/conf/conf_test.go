package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromPathDefaultsClientKeySourceToDeterministicSplit(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	body := []byte(`Runtime:
  Engine: embedded
Nodes:
  - ApiHost: https://panel.example.com
    NodeID: 6
    ApiKey: token
`)
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}

	cfg := New()
	if err := cfg.LoadFromPath(configPath); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeConfig.ClientKeySource != DefaultClientKeySource {
		t.Fatalf("ClientKeySource = %q, want %q", cfg.RuntimeConfig.ClientKeySource, DefaultClientKeySource)
	}
}
