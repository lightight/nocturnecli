package app

// Anonymous, end-to-end-encrypted problem reports.
//
// When a session hits recurring tool-call/stream failures, the CLI offers (via
// a transcript hint and /report) to send the Nocturne team a small debug
// report. Privacy rules, enforced here by construction:
//
//   - strictly opt-in: nothing is ever sent without an explicit /report send
//     (or NOCTURNE_SEND_REPORT=1 in headless mode)
//   - no user-identifiable data: the payload carries only aggregate event
//     counts, an anonymous hiccup timeline (where in the session each hiccup
//     happened: position + category + tool name), tool names, sanitized
//     JSON-decoder error kinds, version/OS/arch, and the model id. No prompts,
//     paths, file contents, commands, args, session IDs, API key, account, or
//     cwd — see the allowlist test.
//   - end-to-end encrypted: the payload is sealed to the team's X25519 public
//     key (embedded below) with an ephemeral key + HKDF-SHA256 + AES-256-GCM.
//     The server only stores the sealed box; it cannot read it.
//   - anonymous transport: the POST carries no Authorization header, so the
//     report cannot be tied to an account.
//
// Server contract (implemented outside this repo): accept unauthenticated
// POST {BaseURL}/api/report with body {"v":1,"alg":...,"box":base64} and store
// the box. The team decrypts offline with `nocturne report-decrypt` and the
// private key from `nocturne report-keygen`.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// reportPublicKeyB64 is the team's X25519 public key. Reports sealed to it can
// only be opened by whoever holds the matching private key. Overridable with
// NOCTURNE_REPORT_PUBKEY for key rotation and tests.
const reportPublicKeyB64 = "4rwWWBSkBoAkrVm3hX26+bVKHzcgMXj73x33MoNl3T0="

const reportAlg = "x25519-hkdfsha256-aes256gcm"

// reportHintThreshold is how many hiccups a session needs before the CLI
// mentions that a report would help. Breaker trips hint immediately.
const reportHintThreshold = 3

// hiccupEvent is one entry in the session timeline: what kind of hiccup and
// where in the session it happened. `At` is the round number in headless mode
// and the message count in the TUI. No content — position + category only.
type hiccupEvent struct {
	At   int    `json:"at"`
	Kind string `json:"kind"`
	Tool string `json:"tool,omitempty"`
}

// maxHiccupEvents caps the timeline so a pathological session can't grow the
// payload without bound.
const maxHiccupEvents = 25

// healthTracker counts reliability hiccups in one session. It never stores
// content — only categories, tool names, sanitized error kinds, and where in
// the session each hiccup happened.
type healthTracker struct {
	counts   map[string]int
	tools    map[string]bool
	jsonErrs map[string]bool
	at       int
	events   []hiccupEvent
}

func newHealthTracker() *healthTracker {
	return &healthTracker{counts: map[string]int{}, tools: map[string]bool{}, jsonErrs: map[string]bool{}}
}

// notePos marks the current session position (message count in the TUI) so
// hiccups recorded afterwards can say where they happened. Headless mode gets
// its position from recordRound instead.
func (h *healthTracker) notePos(at int) { h.at = at }

func (h *healthTracker) add(kind, tool string) {
	h.counts[kind]++
	if len(h.events) < maxHiccupEvents {
		h.events = append(h.events, hiccupEvent{At: h.at, Kind: kind, Tool: tool})
	}
}

// recordBadCall categorizes a call diagnoseBadToolCall rejected.
func (h *healthTracker) recordBadCall(tc ToolCall, diagnosis string) {
	var kind string
	switch {
	case tc.Args["__truncated"] != nil:
		kind = "bad_call_truncated"
	case tc.Args["__parse_error"] != nil:
		kind = "bad_call_json"
		if detail, _ := tc.Args["__err"].(string); detail != "" && len(h.jsonErrs) < 5 {
			h.jsonErrs[redactJSONError(detail)] = true
		}
	case strings.Contains(diagnosis, "not a known tool"):
		kind = "bad_call_unknown_tool"
	default:
		kind = "bad_call_missing_arg"
	}
	name := canonicalTool(tc.Name)
	h.add(kind, name)
	if name != "" {
		h.tools[name] = true
	}
}

func (h *healthTracker) recordEmptyReply()  { h.add("empty_reply", "") }
func (h *healthTracker) recordStreamError() { h.add("stream_error", "") }
func (h *healthTracker) recordBreakerTrip() { h.add("breaker_trip", "") }
func (h *healthTracker) recordAPIError()    { h.add("api_error", "") }
func (h *healthTracker) recordRound() {
	h.counts["rounds"]++
	h.at = h.counts["rounds"]
}

func (h *healthTracker) issues() int {
	n := 0
	for k, v := range h.counts {
		if k != "rounds" {
			n += v
		}
	}
	return n
}

// quotedLiteral matches single-quoted fragments in json.Decoder error strings
// (e.g. invalid character 'F' ...) so they can be redacted — the character
// comes from model output and is not ours to share.
var quotedLiteral = regexp.MustCompile(`'[^']*'`)

// redactJSONError strips any quoted literals from a decoder error, leaving
// only the error kind (e.g. "invalid character '?' in string escape code").
func redactJSONError(s string) string {
	return oneLine(quotedLiteral.ReplaceAllString(s, `'?'`), 120)
}

// reportPayload is the complete inventory of what a report contains. If it is
// not a field here, it is not sent. Keep it free of anything user-identifying.
type reportPayload struct {
	V        int            `json:"v"` // payload schema version
	ID       string         `json:"id"`
	Time     string         `json:"time"`
	Version  string         `json:"version"`
	OS       string         `json:"goos"`
	Arch     string         `json:"goarch"`
	Model    string         `json:"model"`
	Level    string         `json:"level,omitempty"`
	Counts   map[string]int `json:"counts"`
	Hiccups  []hiccupEvent  `json:"hiccups,omitempty"`
	Tools    []string       `json:"tools,omitempty"`
	JSONErrs []string       `json:"json_errors,omitempty"`
	Messages int            `json:"messages"`
	DurSecs  int64          `json:"duration_secs"`
}

// buildReport assembles the payload from aggregate session state only.
func buildReport(h *healthTracker, cfg *Config, version string, messages int, started time.Time) reportPayload {
	var b [6]byte
	_, _ = rand.Read(b[:])
	p := reportPayload{
		V:       1,
		ID:      fmt.Sprintf("%x", b[:]),
		Time:    time.Now().UTC().Format(time.RFC3339),
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Model:   cfg.Model,
		Level:   cfg.Level,
		Counts:  h.counts,
		Hiccups: h.events,
	}
	for t := range h.tools {
		p.Tools = append(p.Tools, t)
	}
	sort.Strings(p.Tools)
	for e := range h.jsonErrs {
		p.JSONErrs = append(p.JSONErrs, e)
	}
	sort.Strings(p.JSONErrs)
	p.Messages = messages
	if !started.IsZero() {
		p.DurSecs = int64(time.Since(started).Seconds())
	}
	return p
}

// reportEnvelope is the sealed wire body posted to the server.
type reportEnvelope struct {
	V   int    `json:"v"`
	Alg string `json:"alg"`
	Box string `json:"box"`
}

func reportRecipientKey() (string, error) {
	pub := strings.TrimSpace(os.Getenv("NOCTURNE_REPORT_PUBKEY"))
	if pub == "" {
		pub = reportPublicKeyB64
	}
	if pub == "REPORT_KEY_NOT_SET" {
		return "", fmt.Errorf("no report public key is baked into this build — the team sets one at build time (or NOCTURNE_REPORT_PUBKEY)")
	}
	if _, err := ecdh.X25519().NewPublicKey(mustB64(pub)); err != nil {
		return "", fmt.Errorf("invalid report public key: %w", err)
	}
	return pub, nil
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// encryptReport seals plaintext to the recipient's X25519 public key using an
// ephemeral keypair, HKDF-SHA256 (salted with both public keys), and
// AES-256-GCM. Wire format: ephPub(32) || nonce(12) || ciphertext.
func encryptReport(plaintext []byte, pubB64 string) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(mustB64(pubB64))
	if err != nil {
		return nil, fmt.Errorf("bad recipient key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, err
	}
	salt := append(append([]byte(nil), eph.PublicKey().Bytes()...), pub.Bytes()...)
	key, err := hkdf.Key(sha256.New, shared, salt, "nocturne-report-v1", 32)
	if err != nil {
		return nil, err
	}
	aead, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	box := append(append([]byte(nil), eph.PublicKey().Bytes()...), nonce...)
	box = aead.Seal(box, nonce, plaintext, nil)
	env := reportEnvelope{V: 1, Alg: reportAlg, Box: base64.StdEncoding.EncodeToString(box)}
	return json.Marshal(env)
}

// decryptReport opens a sealed envelope with the team's private key. Used by
// `nocturne report-decrypt` and by tests.
func decryptReport(envData []byte, privB64 string) ([]byte, error) {
	var env reportEnvelope
	if err := json.Unmarshal(envData, &env); err != nil {
		return nil, fmt.Errorf("bad envelope: %w", err)
	}
	if env.Alg != reportAlg {
		return nil, fmt.Errorf("unknown report algorithm %q", env.Alg)
	}
	box, err := base64.StdEncoding.DecodeString(env.Box)
	if err != nil {
		return nil, err
	}
	priv, err := ecdh.X25519().NewPrivateKey(mustB64(privB64))
	if err != nil {
		return nil, fmt.Errorf("bad private key: %w", err)
	}
	if len(box) < 32+12 {
		return nil, fmt.Errorf("sealed box too short")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(box[:32])
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(ephPub)
	if err != nil {
		return nil, err
	}
	salt := append(append([]byte(nil), ephPub.Bytes()...), priv.PublicKey().Bytes()...)
	key, err := hkdf.Key(sha256.New, shared, salt, "nocturne-report-v1", 32)
	if err != nil {
		return nil, err
	}
	aead, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, box[32:32+aead.NonceSize()], box[32+aead.NonceSize():], nil)
}

func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// generateReportKey creates a fresh X25519 keypair for the team
// (`nocturne report-keygen`): the public half is baked into builds, the
// private half stays with the team.
func generateReportKey() (pub, priv string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()),
		base64.StdEncoding.EncodeToString(k.Bytes()), nil
}

// reportURL is where sealed reports are posted. Unauthenticated by design.
func reportURL(cfg *Config) string {
	if u := strings.TrimSpace(os.Getenv("NOCTURNE_REPORT_URL")); u != "" {
		return u
	}
	return strings.TrimRight(cfg.BaseURL, "/") + "/api/report"
}

// sendReport marshals, seals, and anonymously POSTs the payload.
func sendReport(cfg *Config, p reportPayload) error {
	pub, err := reportRecipientKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return err
	}
	env, err := encryptReport(plain, pub)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, reportURL(cfg), strings.NewReader(string(env)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header: the report is unlinkable to an account.
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("report endpoint returned %s", resp.Status)
	}
	return nil
}
