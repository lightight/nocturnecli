package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// neutralizeEnvKeys clears any real NOCTURNE_* keys so the env doesn't bleed
// into config tests (t.Setenv to "" — envAPIKey skips empty values).
func neutralizeEnvKeys(t *testing.T) {
	t.Helper()
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
}

// A key entered with `/key noct_…` must be written to disk and load back from
// the same path regardless of the working directory.
func TestKeyRoundTrip(t *testing.T) {
	neutralizeEnvKeys(t)
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := &Config{path: path, Model: DefaultModel, BaseURL: DefaultBaseURL, Stream: true}
	cfg.SetAPIKey("noct_secret_abcd1234")
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := loadConfig(path)
	if got.APIKey != "noct_secret_abcd1234" {
		t.Fatalf("reloaded key = %q, want it persisted", got.APIKey)
	}
	if !got.keyPersisted {
		t.Fatal("keyPersisted should be true after loading a saved key")
	}
}

// A key sourced from the environment is not persisted by default, but a bare
// `/key` (PersistKey) promotes it into the private config.
func TestPersistKeyPromotesEnvKey(t *testing.T) {
	neutralizeEnvKeys(t)
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := &Config{path: path, APIKey: "noct_from_env_key99", keyFromEnv: true, Model: DefaultModel, BaseURL: DefaultBaseURL, Stream: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := loadConfig(path); got.APIKey != "" {
		t.Fatalf("env key should not be persisted by default, got %q", got.APIKey)
	}

	cfg.PersistKey() // bare /key
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save after PersistKey: %v", err)
	}
	if got := loadConfig(path); got.APIKey != "noct_from_env_key99" {
		t.Fatalf("PersistKey should persist the key, got %q", got.APIKey)
	}
}

func TestKeyNeedsPersist(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"env, unsaved", Config{APIKey: "noct_x", keyFromEnv: true}, true},
		{"env, already saved", Config{APIKey: "noct_x", keyFromEnv: true, keyPersisted: true}, false},
		{"set via /key", Config{APIKey: "noct_x", keyFromEnv: false}, false},
		{"no key", Config{}, false},
	}
	for _, c := range cases {
		if got := c.cfg.KeyNeedsPersist(); got != c.want {
			t.Errorf("%s: KeyNeedsPersist()=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestMaskedKey(t *testing.T) {
	if got := (&Config{APIKey: "noct_abcdefgh1234"}).MaskedKey(); got != "noct_…1234" {
		t.Errorf("MaskedKey = %q, want noct_…1234", got)
	}
	if got := (&Config{}).MaskedKey(); got != "" {
		t.Errorf("MaskedKey of empty = %q, want empty", got)
	}
	if got := (&Config{APIKey: "short"}).MaskedKey(); got != "…" {
		t.Errorf("MaskedKey of short = %q, want …", got)
	}
	// A masked key must never contain the full secret.
	full := "noct_supersecretvalue"
	if strings.Contains((&Config{APIKey: full}).MaskedKey(), "supersecret") {
		t.Error("MaskedKey leaked the secret body")
	}
}
