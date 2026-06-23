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
