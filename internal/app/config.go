package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Defaults for the Nocturne API.
const (
	DefaultBaseURL = "https://nocturne.lol"
	DefaultModel   = "navy:gpt-5.5"
)

// envKeys are checked, in order, when resolving the API key from the
// environment. The user's .env spells it NOCTURNE_API; the misspelled
// variant and the "_KEY" suffix are tolerated for convenience.
var envKeys = []string{"NOCTURNE_API", "NOCTURNE_API_KEY", "NOCTURE_API"}

// Config holds everything the CLI needs to talk to the API. It is loaded
// from (in increasing precedence) the on-disk config file, a local .env
// file, and finally real process environment variables.
type Config struct {
	APIKey      string   `json:"api_key,omitempty"`
	Model       string   `json:"model,omitempty"`
	BaseURL     string   `json:"base_url,omitempty"`
	Stream      bool     `json:"stream"`                // live-stream replies (default true)
	Level       string   `json:"level,omitempty"`       // thinking: off · normal · extended
	Temperature *float64 `json:"temperature,omitempty"` // 0–2 (unset = API default)

	path       string // resolved config-file path (not serialized)
	keyFromEnv bool   // true when APIKey came from the environment
}

type persisted struct {
	APIKey      string   `json:"api_key,omitempty"`
	Model       string   `json:"model,omitempty"`
	BaseURL     string   `json:"base_url,omitempty"`
	Stream      *bool    `json:"stream,omitempty"` // pointer so "absent" stays default-on
	Level       string   `json:"level,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// configDir returns the directory that holds Nocturne's config file,
// honouring the platform conventions (XDG on Linux, AppData on Windows,
// ~/Library/Application Support on macOS).
func configDir() string {
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return filepath.Join(d, "nocturne")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nocturne")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// LoadConfig reads the persisted config, layers a local .env on top, then
// lets real environment variables win for the API key.
func LoadConfig() *Config {
	loadDotEnv(".env")

	cfg := &Config{Model: DefaultModel, BaseURL: DefaultBaseURL, Stream: true}
	cfg.path = configPath()

	if data, err := os.ReadFile(cfg.path); err == nil {
		var p persisted
		if json.Unmarshal(data, &p) == nil {
			if p.Model != "" {
				cfg.Model = normalizeModelID(p.Model)
			}
			if p.BaseURL != "" {
				cfg.BaseURL = p.BaseURL
			}
			if p.Stream != nil {
				cfg.Stream = *p.Stream
			}
			cfg.Level = p.Level
			cfg.Temperature = p.Temperature
			cfg.APIKey = p.APIKey
		}
	}

	if k := envAPIKey(); k != "" {
		cfg.APIKey = k
		cfg.keyFromEnv = true
	}
	return cfg
}

func normalizeModelID(id string) string {
	id = strings.TrimSpace(id)
	switch id {
	case "gpt-5.5":
		return "navy:gpt-5.5"
	default:
		return id
	}
}

func envAPIKey() string {
	for _, name := range envKeys {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// SetAPIKey records a key entered interactively so it survives Save().
func (c *Config) SetAPIKey(k string) {
	c.APIKey = strings.TrimSpace(k)
	c.keyFromEnv = false
}

// Save writes model/base-url (and a manually-entered key) back to disk. A
// key sourced from the environment is never persisted, to avoid copying a
// secret the user is already managing elsewhere.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	p := persisted{Model: normalizeModelID(c.Model), BaseURL: c.BaseURL, Stream: &c.Stream, Level: c.Level, Temperature: c.Temperature}
	if !c.keyFromEnv {
		p.APIKey = c.APIKey
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}

// loadDotEnv parses a KEY=VALUE file and sets any vars not already present
// in the environment. Quotes and a leading `export ` are stripped.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); !ok {
			_ = os.Setenv(key, val)
		}
	}
}
