package app

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateJSONUsesClientPlatform(t *testing.T) {
	s := newRelayServer(ServerAssets{UpdateVersion: "v9.9.9"})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	var body struct {
		Version string `json:"version"`
		URL     string `json:"url"`
	}
	get := func(target string) {
		resp, err := srv.Client().Get(srv.URL + target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer resp.Body.Close()
		body.Version, body.URL = "", ""
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
	}

	get("/update.json?os=darwin&arch=arm64")
	if want := srv.URL + "/bin/nocturne_darwin_arm64"; body.URL != want {
		t.Errorf("darwin/arm64 url = %q, want %q", body.URL, want)
	}

	get("/update.json?os=windows&arch=amd64")
	if want := srv.URL + "/bin/nocturne_windows_amd64.exe"; body.URL != want {
		t.Errorf("windows/amd64 url = %q, want %q", body.URL, want)
	}

	// Without (or with bogus) platform params the URL is omitted so the client
	// falls back to its own platform's asset instead of the server's.
	get("/update.json")
	if body.URL != "" {
		t.Errorf("no params: url = %q, want empty", body.URL)
	}
	get("/update.json?os=plan9&arch=mips")
	if body.URL != "" {
		t.Errorf("bogus platform: url = %q, want empty", body.URL)
	}
}

func TestUpdateEndpointsUseAdvertisedVersion(t *testing.T) {
	s := newRelayServer(ServerAssets{UpdateVersion: "v1.2.3"})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(body), "v1.2.3\n"; got != want {
		t.Fatalf("/version = %q, want %q", got, want)
	}

	resp, err = srv.Client().Get(srv.URL + "/update.json")
	if err != nil {
		t.Fatalf("GET /update.json: %v", err)
	}
	defer resp.Body.Close()
	var u struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		t.Fatalf("decode /update.json: %v", err)
	}
	if u.Version != "v1.2.3" {
		t.Fatalf("/update.json version = %q, want v1.2.3", u.Version)
	}
}

func TestReadUpdateVersionFile(t *testing.T) {
	dir := t.TempDir()
	if got := readUpdateVersionFile(dir); got != "" {
		t.Fatalf("empty dir version = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("v4.5.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readUpdateVersionFile(dir); got != "v4.5.6" {
		t.Fatalf("version file = %q, want v4.5.6", got)
	}
}

func TestValidPlatform(t *testing.T) {
	for _, p := range [][2]string{
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"linux", "amd64"}, {"linux", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		if !validPlatform(p[0], p[1]) {
			t.Errorf("validPlatform(%s, %s) = false, want true", p[0], p[1])
		}
	}
	for _, p := range [][2]string{
		{"plan9", "amd64"}, {"linux", "mips"}, {"", ""}, {"darwin", ""},
	} {
		if validPlatform(p[0], p[1]) {
			t.Errorf("validPlatform(%s, %s) = true, want false", p[0], p[1])
		}
	}
}
