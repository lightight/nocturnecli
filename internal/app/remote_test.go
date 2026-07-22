package app

import (
	"strings"
	"testing"
)

func TestRemoteCryptoRoundTrip(t *testing.T) {
	h := &remoteHub{key: deriveRemoteKey("ABC234")}
	ct := h.encrypt([]byte("secret payload"))
	pt, err := h.decrypt(ct)
	if err != nil || string(pt) != "secret payload" {
		t.Fatalf("round-trip failed: %v %q", err, pt)
	}
	// A different pairing code must not be able to decrypt.
	wrong := &remoteHub{key: deriveRemoteKey("XYZ789")}
	if _, err := wrong.decrypt(ct); err == nil {
		t.Fatal("wrong code decrypted — E2EE broken")
	}
}

func TestPairingCode(t *testing.T) {
	c := newPairingCode()
	if len(c) != 6 {
		t.Fatalf("code length = %d, want 6", len(c))
	}
	if strings.ContainsAny(c, "IO01") {
		t.Fatalf("ambiguous chars in code: %s", c)
	}
}

func TestAppendRemoteEventCoalesces(t *testing.T) {
	batch := []remoteEvent{{Kind: "stream", Text: "a"}}
	batch = appendRemoteEvent(batch, remoteEvent{Kind: "stream", Text: "ab"})
	if len(batch) != 1 || batch[0].Text != "ab" {
		t.Fatalf("stream events not coalesced: %+v", batch)
	}
	// A different kind between streams must not be merged over.
	batch = appendRemoteEvent(batch, remoteEvent{Kind: "tool", Text: "t"})
	batch = appendRemoteEvent(batch, remoteEvent{Kind: "stream", Text: "abc"})
	if len(batch) != 3 {
		t.Fatalf("distinct kinds merged: %+v", batch)
	}
	// Drafts coalesce the same way.
	batch = appendRemoteEvent(batch, remoteEvent{Kind: "input", Text: "h"})
	batch = appendRemoteEvent(batch, remoteEvent{Kind: "input", Text: "hi"})
	if len(batch) != 4 || batch[3].Text != "hi" {
		t.Fatalf("input drafts not coalesced: %+v", batch)
	}
}
