package app

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// The browser ⇄ terminal link is end-to-end encrypted with AES-256-GCM, using a
// key derived from the pairing code via PBKDF2-HMAC-SHA256. The relay only ever
// sees ciphertext; whoever types the code in the browser derives the same key.
var remoteSalt = []byte("nocturne-remote-v1")

const (
	remoteIters  = 150_000
	defaultRelay = "https://nocturnecli.lol"
)

// relayBase is where the CLI registers remote sessions (override for testing).
func relayBase() string {
	if v := strings.TrimSpace(os.Getenv("NOCTURNE_RELAY")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultRelay
}

// remoteEvent is one update relayed between the terminal and the browser.
type remoteEvent struct {
	Kind string `json:"kind"` // hello, system, user, stream, assistant, tool, status
	Text string `json:"text"`
}

// remoteSubmitMsg is delivered to the TUI when the browser sends a message.
type remoteSubmitMsg struct{ text string }

// remoteHub is the CLI's client of the relay for one remote session.
type remoteHub struct {
	code string
	key  []byte
	base string
	id   string
	url  string

	send   func(remoteSubmitMsg)
	out    chan remoteEvent
	cancel context.CancelFunc
	httpc  *http.Client
}

func deriveRemoteKey(code string) []byte {
	k, _ := pbkdf2.Key(sha256.New, code, remoteSalt, remoteIters, 32)
	return k
}

func newPairingCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// startRemote registers a session with the relay and starts pumping events.
func startRemote(send func(remoteSubmitMsg)) (*remoteHub, error) {
	base := relayBase()
	client := &http.Client{} // no global timeout: SSE is long-lived, POSTs use contexts

	req, err := http.NewRequest(http.MethodPost, base+"/api/remote/new", nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("can't reach relay %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("relay returned %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.ID == "" {
		return nil, fmt.Errorf("relay gave no session id")
	}

	code := newPairingCode()
	h := &remoteHub{
		code: code, key: deriveRemoteKey(code),
		base: base, id: out.ID, url: base + "/r/" + out.ID,
		send: send, out: make(chan remoteEvent, 256), httpc: client,
	}
	rctx, rcancel := context.WithCancel(context.Background())
	h.cancel = rcancel
	go h.sender(rctx)
	go h.listener(rctx)
	return h, nil
}

func (h *remoteHub) Stop() {
	if h == nil {
		return
	}
	h.cancel()
}

// broadcast queues an event for the browser (non-blocking, drops if backed up).
func (h *remoteHub) broadcast(ev remoteEvent) {
	if h == nil {
		return
	}
	select {
	case h.out <- ev:
	default:
	}
}

// sender drains the outbound queue, posting encrypted events to the relay.
func (h *remoteHub) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.out:
			data, _ := json.Marshal(ev)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				h.base+"/api/remote/"+h.id+"/from-cli", strings.NewReader(h.encrypt(data)))
			if err != nil {
				continue
			}
			if resp, err := h.httpc.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
}

// listener subscribes to browser→CLI messages, reconnecting on drop.
func (h *remoteHub) listener(ctx context.Context) {
	for ctx.Err() == nil {
		h.streamOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *remoteHub) streamOnce(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/api/remote/"+h.id+"/to-cli", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.httpc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		plain, err := h.decrypt(payload)
		if err != nil {
			continue // not for us / wrong code
		}
		var ev remoteEvent
		if json.Unmarshal(plain, &ev) != nil {
			continue
		}
		if ev.Kind == "hello" {
			h.broadcast(remoteEvent{Kind: "system", Text: "connected"})
			continue
		}
		if strings.TrimSpace(ev.Text) != "" {
			h.send(remoteSubmitMsg{text: ev.Text})
		}
	}
}

// --- crypto ----------------------------------------------------------------

func (h *remoteHub) encrypt(plain []byte) string {
	block, _ := aes.NewCipher(h.key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil))
}

func (h *remoteHub) decrypt(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(h.key)
	gcm, _ := cipher.NewGCM(block)
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, raw[:ns], raw[ns:], nil)
}
