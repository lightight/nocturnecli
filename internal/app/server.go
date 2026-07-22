package app

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServerAssets are the static files baked into the binary and served by
// `nocturne serve` (the docs landing page + install scripts).
type ServerAssets struct {
	Docs       string
	InstallSh  string
	InstallPs1 string
	BinDir     string // optional directory of prebuilt binaries to serve at /bin/
}

// RunServer hosts the docs site + install scripts + the end-to-end-encrypted
// remote-control relay. It speaks plain HTTP — front it with a reverse proxy
// (Caddy/Cloudflare) for HTTPS, which the browser remote UI requires.
func RunServer(args []string, assets ServerAssets) error {
	addr := ":8080"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-addr", "--addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			}
		case "-bin", "--bin":
			if i+1 < len(args) {
				assets.BinDir = args[i+1]
				i++
			}
		}
	}
	srv := newRelayServer(assets)
	fmt.Printf("Nocturne site + relay listening on %s\n", addr)
	return http.ListenAndServe(addr, srv.handler())
}

// --- relay -----------------------------------------------------------------

type fanout struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newFanout() *fanout { return &fanout{subs: map[chan string]struct{}{}} }

func (f *fanout) sub() chan string {
	ch := make(chan string, 128)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch
}

func (f *fanout) unsub(ch chan string) {
	f.mu.Lock()
	if _, ok := f.subs[ch]; ok {
		delete(f.subs, ch)
		close(ch)
	}
	f.mu.Unlock()
}

func (f *fanout) publish(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- s:
		default:
		}
	}
}

type relaySession struct {
	toCLI     *fanout // browser → CLI
	toBrowser *fanout // CLI → browser
	lastSeen  time.Time
}

type relayServer struct {
	assets   ServerAssets
	mu       sync.Mutex
	sessions map[string]*relaySession
}

func newRelayServer(assets ServerAssets) *relayServer {
	s := &relayServer{assets: assets, sessions: map[string]*relaySession{}}
	go s.reap()
	return s
}

// reap drops sessions idle for over 30 minutes.
func (s *relayServer) reap() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-30 * time.Minute)
		s.mu.Lock()
		for id, sess := range s.sessions {
			if sess.lastSeen.Before(cutoff) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *relayServer) get(id string) *relaySession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess != nil {
		sess.lastSeen = time.Now()
	}
	return sess
}

func (s *relayServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /install.sh", s.installSh)
	mux.HandleFunc("GET /install.ps1", s.installPs1)
	mux.HandleFunc("GET /r/{id}", s.remoteUI)
	mux.HandleFunc("POST /api/remote/new", s.newSession)
	mux.HandleFunc("GET /api/remote/{id}/to-cli", s.toCLI)
	mux.HandleFunc("GET /api/remote/{id}/to-browser", s.toBrowser)
	mux.HandleFunc("POST /api/remote/{id}/from-cli", s.fromCLI)
	mux.HandleFunc("POST /api/remote/{id}/from-browser", s.fromBrowser)
	if s.assets.BinDir != "" {
		mux.Handle("GET /bin/", http.StripPrefix("/bin/", http.FileServer(http.Dir(s.assets.BinDir))))
	}
	return mux
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *relayServer) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, s.assets.Docs)
}

func (s *relayServer) installSh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	io.WriteString(w, strings.ReplaceAll(s.assets.InstallSh, "__BASE__", baseURL(r)))
}

func (s *relayServer) installPs1(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, strings.ReplaceAll(s.assets.InstallPs1, "__BASE__", baseURL(r)))
}

func (s *relayServer) remoteUI(w http.ResponseWriter, r *http.Request) {
	if s.get(r.PathValue("id")) == nil {
		http.Error(w, "session not found or expired", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, remoteHTML) // the JS reads the session id from the URL path
}

func (s *relayServer) newSession(w http.ResponseWriter, r *http.Request) {
	id := randomID()
	s.mu.Lock()
	s.sessions[id] = &relaySession{toCLI: newFanout(), toBrowser: newFanout(), lastSeen: time.Now()}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (s *relayServer) toCLI(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	serveSSE(w, r, sess.toCLI)
}

func (s *relayServer) toBrowser(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	serveSSE(w, r, sess.toBrowser)
}

func (s *relayServer) fromCLI(w http.ResponseWriter, r *http.Request) {
	s.relay(w, r, func(sess *relaySession, body string) { sess.toBrowser.publish(body) })
}

func (s *relayServer) fromBrowser(w http.ResponseWriter, r *http.Request) {
	s.relay(w, r, func(sess *relaySession, body string) { sess.toCLI.publish(body) })
}

func (s *relayServer) relay(w http.ResponseWriter, r *http.Request, push func(*relaySession, string)) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if b := strings.TrimSpace(string(body)); b != "" {
		push(sess, b)
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveSSE(w http.ResponseWriter, r *http.Request, f *fanout) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := f.sub()
	defer f.unsub(ch)
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
