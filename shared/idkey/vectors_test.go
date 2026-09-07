package idkey

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFingerprintVectors emits fingerprints for a fixed set of public keys
// so the browser implementation can be checked against this one.
//
// The fingerprint is shown on both sides — the PC displays the client's, the
// client displays the PC's — and the entire point of it is that a user can
// compare the two by eye. Two implementations that disagree would render that
// comparison meaningless while looking, to a user, exactly like a security
// warning. Nothing else in the test suite would catch it.
func TestWriteFingerprintVectors(t *testing.T) {
	// Deterministic keys: a stable corpus, not fresh randomness. These are
	// public keys, so there is nothing here to keep secret.
	seeds := []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		"KiXpQLl3S3ss9y0ZfmRmJTKCF1rQXt8BajlJzftH2Yw",
		"8COTdroxtOZOFB2k68kNWXWlsTs4Qr1kL0qfBPYLZOc",
		"WZB9D87fF4fSl646fcfe-T_G212BzXw9yFEUUp9ixmY",
		"Ec7K9BiwCFXrcGCx0OLK_tLgPfDaC_guJydmuyqgwWA",
		"TwQntug_47FtyBZ9GF6ZTXX4xO3iI-jyNnKMeQIW_Gw",
		"PUjtO0aIGoJAEMJtgq3q2fApv-j8I5btPDeFMpk1NBo",
		"__________________________________________8",
	}

	out := map[string]any{"alphabet": FingerprintAlphabet}
	keys := make([]map[string]string, 0, len(seeds))
	for _, s := range seeds {
		pub, err := ParsePublic(s)
		if err != nil {
			t.Fatalf("ParsePublic(%q): %v", s, err)
		}
		keys = append(keys, map[string]string{
			"publicKey":   pub.String(),
			"fingerprint": pub.Fingerprint(),
		})
	}
	out["keys"] = keys

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join("testdata", "fingerprints.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
