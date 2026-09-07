package pair

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mmc/all-share/shared/idkey"
)

// TestWriteInteropVectors emits a fixed pairing exchange for the browser test
// suite to reproduce. Two independent implementations of a key exchange that
// disagree would surface to a user as an unexplainable "that code did not
// work", so they are pinned against each other rather than trusted to match.
func TestWriteInteropVectors(t *testing.T) {
	// Every input is deterministic, keys included. An earlier version generated
	// fresh keys on each run, which rewrote this file on every `go test` and
	// buried real changes in churn. A fixed corpus also means a browser-side
	// regression shows up as a diff against known-good values rather than as a
	// mismatch that could be either implementation's fault.
	const code = "K7M2Q9XR4TVZ"
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	codeID, err := DeriveCodeID(code)
	if err != nil {
		t.Fatalf("code id: %v", err)
	}
	password, err := DerivePassword(code, salt)
	if err != nil {
		t.Fatalf("password: %v", err)
	}

	agentEph, err := ephemeralFromSeed("allshare-interop-agent-ephemeral")
	if err != nil {
		t.Fatalf("agent ephemeral: %v", err)
	}
	clientEph, err := ephemeralFromSeed("allshare-interop-client-ephemeral")
	if err != nil {
		t.Fatalf("client ephemeral: %v", err)
	}

	shared, err := agentEph.Shared(clientEph.PublicBytes())
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}

	// Only the public halves take part in the transcript, so the vectors pin
	// two fixed public keys rather than deriving a private key the corpus does
	// not need.
	agentIDPub, err := idkey.ParsePublic(fixedAgentIDPub)
	if err != nil {
		t.Fatalf("agent identity: %v", err)
	}
	clientIDPub, err := idkey.ParsePublic(fixedClientIDPub)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}

	transcript := Transcript{
		CodeID: codeID, Salt: salt,
		AgentEPK: agentEph.PublicBytes(), ClientEPK: clientEph.PublicBytes(),
		AgentIDPub: agentIDPub, ClientIDPub: clientIDPub,
		DeviceName: "My Gaming PC", ClientLabel: "Chromebook",
	}
	master, err := Master(shared, password, transcript)
	if err != nil {
		t.Fatalf("master: %v", err)
	}

	out := map[string]string{
		"code":           code,
		"codeId":         codeID,
		"salt":           idkey.EncodeB64(salt),
		"password":       idkey.EncodeB64(password),
		"agentEpk":       idkey.EncodeB64(agentEph.PublicBytes()),
		"clientEpk":      idkey.EncodeB64(clientEph.PublicBytes()),
		"agentIdPub":     idkey.EncodeB64(agentIDPub.Bytes()),
		"clientIdPub":    idkey.EncodeB64(clientIDPub.Bytes()),
		"deviceName":     "My Gaming PC",
		"clientLabel":    "Chromebook",
		"shared":         idkey.EncodeB64(shared),
		"transcriptHash": idkey.EncodeB64(transcript.hash()),
		"master":         idkey.EncodeB64(master),
		"confirmClient":  idkey.EncodeB64(ConfirmTag(master, ClientRole)),
		"confirmAgent":   idkey.EncodeB64(ConfirmTag(master, AgentRole)),
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "interop.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Fixed identity public keys for the corpus. They are public keys, so there is
// nothing here to keep secret; they exist only to make the transcript, and
// therefore every derived value in interop.json, reproducible.
const (
	fixedAgentIDPub  = "KiXpQLl3S3ss9y0ZfmRmJTKCF1rQXt8BajlJzftH2Yw"
	fixedClientIDPub = "8COTdroxtOZOFB2k68kNWXWlsTs4Qr1kL0qfBPYLZOc"
)

// ephemeralFromSeed derives a fixed X25519 key from a label, so the vectors are
// reproducible byte for byte. Test-only: real ephemerals come from
// NewEphemeral and crypto/rand.
func ephemeralFromSeed(label string) (Ephemeral, error) {
	sum := sha256.Sum256([]byte(label))
	priv, err := ecdh.X25519().NewPrivateKey(sum[:])
	if err != nil {
		return Ephemeral{}, err
	}
	return Ephemeral{priv: priv}, nil
}
