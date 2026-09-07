package rvclient

import (
	"testing"

	"github.com/mmc/all-share/shared/idkey"
)

// The service address is the one setting a user types by hand, and getting it
// wrong means signalling — the SDP with every address your devices know, plus
// the relay credentials for the session — travels in the clear. The Go and
// JavaScript implementations of this rule must agree, so the cases here mirror
// those in test/tools/client-unit.js exactly.
func TestValidateEndpoint(t *testing.T) {
	accept := []string{
		"wss://rv.example.com/rv",
		"wss://192.168.1.10:8443/rv",
		"ws://127.0.0.1:8080/rv",
		"ws://localhost:8080/rv",
		"ws://pc.local/rv",
		"ws://192.168.1.10:8080/rv",
		"ws://10.1.2.3/rv",
		"ws://172.20.0.1/rv",
		"ws://169.254.1.1/rv",
		"ws://[::1]:8080/rv",
		// RFC 6598 shared address space: where a mesh VPN puts its devices, and
		// the only route to your own PC for someone who cannot host a server.
		"ws://100.64.0.1/rv",
		"ws://100.101.102.103:8443/rv",
		"ws://100.127.255.254/rv",
	}
	for _, url := range accept {
		if err := ValidateEndpoint(url); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", url, err)
		}
	}

	reject := []string{
		"ws://rv.example.com/rv",
		"ws://172.15.0.1/rv",
		"ws://172.32.0.1/rv",
		"ws://8.8.8.8/rv",
		// Either side of 100.64.0.0/10 is ordinary public space.
		"ws://100.63.255.255/rv",
		"ws://100.128.0.1/rv",
		"ws://100.200.1.1/rv",
		"ws://[2001:db8::1]:8080/rv",
		"ws://10.example.com/rv",
		"http://rv.example.com/rv",
		"https://rv.example.com/rv",
		"rv.example.com/rv",
		"",
	}
	for _, url := range reject {
		if err := ValidateEndpoint(url); err == nil {
			t.Errorf("ValidateEndpoint(%q) = nil, want an error", url)
		}
	}
}

// New must apply the policy, not just expose it: a bad address configured in a
// file should fail at construction rather than at the first dial attempt.
func TestNewRejectsUnencryptedEndpoint(t *testing.T) {
	key, err := idkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{URL: "ws://rv.example.com/rv", Identity: key}); err == nil {
		t.Fatal("New accepted an unencrypted public address")
	}
	if _, err := New(Config{URL: "ws://127.0.0.1:9/rv", Identity: key}); err != nil {
		t.Fatalf("New rejected a loopback address: %v", err)
	}
}
