package pair

import (
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
	// Deterministic inputs: the point is a stable corpus, not fresh randomness.
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

	agentEph, err := NewEphemeral()
	if err != nil {
		t.Fatalf("agent ephemeral: %v", err)
	}
	clientEph, err := NewEphemeral()
	if err != nil {
		t.Fatalf("client ephemeral: %v", err)
	}
	shared, err := agentEph.Shared(clientEph.PublicBytes())
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}

	agentID, _ := idkey.Generate()
	clientID, _ := idkey.Generate()

	transcript := Transcript{
		CodeID: codeID, Salt: salt,
		AgentEPK: agentEph.PublicBytes(), ClientEPK: clientEph.PublicBytes(),
		AgentIDPub: agentID.Public(), ClientIDPub: clientID.Public(),
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
		"agentIdPub":     idkey.EncodeB64(agentID.Public().Bytes()),
		"clientIdPub":    idkey.EncodeB64(clientID.Public().Bytes()),
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
