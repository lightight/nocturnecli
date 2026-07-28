package app

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Defaults for the Nocturne API.
const (
	DefaultBaseURL = "https://nocturne.lol"
	DefaultModel   = "navy:gpt-5.5"
)

// Permission modes govern how mutating tool calls (edits, writes, commands) are
// approved:
//   - PermAsk    confirm every action in the terminal (default, safest).
//   - PermSmart  a small guard model auto-accepts safe actions and only asks
//     about risky ones ("auto-accept").
//   - PermBypass run everything with no checks — set only via /permissions.
const (
	PermAsk    = "ask"
	PermSmart  = "smart"
	PermBypass = "bypass"
)

// normalizePerm coerces a stored/typed permission value to a known mode.
func normalizePerm(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case PermSmart, "auto", "safe":
		return PermSmart
	case PermBypass, "all", "yolo":
		return PermBypass
	default:
		return PermAsk
	}
}

// envKeys are checked, in order, when resolving the API key from the
// environment. The user's .env spells it NOCTURNE_API; the misspelled
// variant and the "_KEY" suffix are tolerated for convenience.
var envKeys = []string{"NOCTURNE_API", "NOCTURNE_API_KEY", "NOCTURE_API"}

// Config holds everything the CLI needs to talk to the API. The API key is
// resolved with this precedence (highest first): a real process environment
// variable, the on-disk config file (saved with /key), then a project-local
// .env file. The .env ranks last so a stale project .env can't silently
// override a key the user deliberately saved.
type Config struct {
	APIKey        string       `json:"api_key,omitempty"`
	Model         string       `json:"model,omitempty"`
	BaseURL       string       `json:"base_url,omitempty"`
	Stream        bool         `json:"stream"`                   // live-stream replies (default true)
	Level         string       `json:"level,omitempty"`          // thinking: off · normal · extended
	Perm          string       `json:"perm,omitempty"`           // approval mode: ask · smart · bypass
	Temperature   *float64     `json:"temperature,omitempty"`    // 0–2 (unset = API default)
	Trusted       []string     `json:"trusted,omitempty"`        // absolute paths the user has trusted
	Tools         []CustomTool `json:"tools,omitempty"`          // user-installed shell-backed tools
	ReportOptOut  bool         `json:"report_optout,omitempty"`  // never show anonymous-report hints
	SkipQuestions bool         `json:"skip_questions,omitempty"` // auto-skip AI ask-tool popups

	path           string // resolved config-file path (not serialized)
	keyFromEnv     bool   // true when APIKey came from the environment or a .env
	keyFromRealEnv bool   // true when APIKey came from a real process env var
	keyPersisted   bool   // true when a key was already present in the config file
}

type persisted struct {
	// APIKey is the legacy plaintext field. New saves write APIKeyEnc instead;
	// loads still accept APIKey so existing users migrate automatically after the
	// next Save().
	APIKey        string       `json:"api_key,omitempty"`
	APIKeyEnc     string       `json:"api_key_enc,omitempty"`
	APIKeyHash    string       `json:"api_key_hash,omitempty"`
	Model         string       `json:"model,omitempty"`
	BaseURL       string       `json:"base_url,omitempty"`
	Stream        *bool        `json:"stream,omitempty"` // pointer so "absent" stays default-on
	Level         string       `json:"level,omitempty"`
	Perm          string       `json:"perm,omitempty"`
	Temperature   *float64     `json:"temperature,omitempty"`
	Trusted       []string     `json:"trusted,omitempty"`
	Tools         []CustomTool `json:"tools,omitempty"`
	ReportOptOut  bool         `json:"report_optout,omitempty"`
	SkipQuestions bool         `json:"skip_questions,omitempty"`
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

// LoadConfig reads the persisted config, then applies the key precedence:
// real environment > saved config > local .env.
func LoadConfig() *Config { return loadConfig(configPath()) }

// loadConfig is the path-parameterized core of LoadConfig, split out so tests
// can exercise the persist/reload round-trip against a temp file.
func loadConfig(path string) *Config {
	// Capture a key from the real process environment BEFORE loadDotEnv runs:
	// .env values are loaded into the environment too, and would otherwise be
	// indistinguishable from the real thing.
	realEnvKey := envAPIKey()

	loadDotEnv(".env")

	cfg := &Config{Model: DefaultModel, BaseURL: DefaultBaseURL, Stream: true, Perm: PermAsk}
	cfg.path = path

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
			if p.Perm != "" {
				cfg.Perm = normalizePerm(p.Perm)
			}
			cfg.Level = p.Level
			cfg.Temperature = p.Temperature
			cfg.Trusted = p.Trusted
			cfg.Tools = normalizeCustomTools(p.Tools)
			cfg.ReportOptOut = p.ReportOptOut
			cfg.SkipQuestions = p.SkipQuestions
			if p.APIKeyEnc != "" {
				if k, err := decryptAPIKey(p.APIKeyEnc); err == nil && k != "" {
					cfg.APIKey = k
					cfg.keyPersisted = true
				}
			} else if p.APIKey != "" {
				// Legacy plaintext config: load it for compatibility. The next Save()
				// writes it back encrypted and omits this field.
				cfg.APIKey = p.APIKey
				cfg.keyPersisted = true
			}
		}
	}

	switch {
	case realEnvKey != "":
		// An explicitly exported variable always wins.
		cfg.APIKey = realEnvKey
		cfg.keyFromEnv = true
		cfg.keyFromRealEnv = true
	case cfg.APIKey != "":
		// The key saved with /key outranks a project-local .env.
	default:
		// No saved key: fall back to whatever the local .env provided.
		cfg.APIKey = envAPIKey()
		cfg.keyFromEnv = cfg.APIKey != ""
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

// trustKey canonicalizes a directory path for trust comparisons: absolute,
// cleaned, and with a consistent case on case-insensitive platforms (Windows).
func trustKey(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	return dir
}

// IsTrusted reports whether the user has already trusted the given workspace
// directory in a previous session.
func (c *Config) IsTrusted(dir string) bool {
	key := trustKey(dir)
	for _, t := range c.Trusted {
		if trustKey(t) == key {
			return true
		}
	}
	return false
}

// Trust records dir as a trusted workspace and persists it, so the trust prompt
// only appears the first time Nocturne runs in a folder. A no-op if already
// trusted.
func (c *Config) Trust(dir string) error {
	if c.IsTrusted(dir) {
		return nil
	}
	abs := dir
	if a, err := filepath.Abs(dir); err == nil {
		abs = filepath.Clean(a)
	}
	c.Trusted = append(c.Trusted, abs)
	return c.Save()
}

// SetAPIKey records a key entered interactively so it survives Save().
func (c *Config) SetAPIKey(k string) {
	c.APIKey = strings.TrimSpace(k)
	c.keyFromEnv = false
}

// PersistKey marks the currently-loaded key to be written to the private
// config file on the next Save, even if it originated from the environment or
// a local .env. This backs a bare `/key`: "remember the key I'm already using,
// everywhere" — once saved, LoadConfig finds it from any working directory.
func (c *Config) PersistKey() { c.keyFromEnv = false }

// MaskedKey returns a redacted form of the API key for display in
// confirmations and status lines. It never reveals the full secret.
func (c *Config) MaskedKey() string {
	k := c.APIKey
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "…"
	}
	return k[:5] + "…" + k[len(k)-4:]
}

// KeyNeedsPersist reports whether a key is loaded from the environment or a
// local .env but isn't yet saved in the private config — i.e. running `/key`
// would make it available from every directory.
func (c *Config) KeyNeedsPersist() bool {
	return c.APIKey != "" && c.keyFromEnv && !c.keyPersisted
}

// ConfigPath reports where the persisted config (including a saved key) lives,
// so callers can tell the user exactly where their key was written.
func ConfigPath() string { return configPath() }

// Save writes model/base-url (and a manually-entered key) back to disk. A
// key sourced from the environment is never persisted, to avoid copying a
// secret the user is already managing elsewhere.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	p := persisted{Model: normalizeModelID(c.Model), BaseURL: c.BaseURL, Stream: &c.Stream, Level: c.Level, Perm: normalizePerm(c.Perm), Temperature: c.Temperature, Trusted: c.Trusted, Tools: normalizeCustomTools(c.Tools), ReportOptOut: c.ReportOptOut, SkipQuestions: c.SkipQuestions}
	if !c.keyFromEnv && c.APIKey != "" {
		enc, err := encryptAPIKey(c.APIKey)
		if err != nil {
			return err
		}
		p.APIKeyEnc = enc
		p.APIKeyHash = hashAPIKey(c.APIKey)
		c.keyPersisted = true
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}

const encryptedAPIKeyPrefix = "nocturne:v1:"

func hashAPIKey(k string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(k)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func apiKeyEncryptionKey() [32]byte {
	home, _ := os.UserHomeDir()
	userCfg, _ := os.UserConfigDir()
	seed := strings.Join([]string{
		"nocturne-cli-api-key-v1",
		runtime.GOOS,
		runtime.GOARCH,
		home,
		userCfg,
	}, "\x00")
	return sha256.Sum256([]byte(seed))
}

func encryptAPIKey(k string) (string, error) {
	k = strings.TrimSpace(k)
	if k == "" {
		return "", nil
	}
	key := apiKeyEncryptionKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("encrypt API key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt API key: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt API key: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(k), nil)
	blob := append(nonce, sealed...)
	return encryptedAPIKeyPrefix + base64.RawURLEncoding.EncodeToString(blob), nil
}

func decryptAPIKey(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, encryptedAPIKeyPrefix) {
		return "", errors.New("unsupported encrypted API key format")
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, encryptedAPIKeyPrefix))
	if err != nil {
		return "", fmt.Errorf("decrypt API key: %w", err)
	}
	key := apiKeyEncryptionKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("decrypt API key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt API key: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return "", errors.New("decrypt API key: ciphertext too short")
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt API key: %w", err)
	}
	return string(plain), nil
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
