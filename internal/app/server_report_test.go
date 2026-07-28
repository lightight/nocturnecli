package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServerReportEndpoint(t *testing.T) {
	dir := t.TempDir()
	srv := newRelayServer(ServerAssets{ReportsDir: dir})

	// Seal a payload exactly the way the client does.
	pub, priv, err := generateReportKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(reportPayload{V: 1, ID: "abc123", Model: "navy:gpt-5.5",
		Counts: map[string]int{"bad_call_json": 2}})
	env, err := encryptReport(payload, pub)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(env))
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// One file landed, and the team key opens it.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("stored reports: %v %v", entries, err)
	}
	blob, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	plain, err := decryptReport(blob, priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte(`"bad_call_json":2`)) {
		t.Fatalf("decrypted report wrong: %s", plain)
	}

	// Garbage is rejected and stores nothing.
	rec = httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader([]byte("hello"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage got %d", rec.Code)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("garbage was stored: %v", entries)
	}
}

func TestServerReportDisabled(t *testing.T) {
	srv := newRelayServer(ServerAssets{})
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader([]byte(`{"v":1,"alg":"x","box":"y"}`))))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled server got %d", rec.Code)
	}
}
