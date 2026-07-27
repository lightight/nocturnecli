package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestUpdateJSONUsesClientPlatform(t *testing.T) {
	s := newRelayServer(ServerAssets{})
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
