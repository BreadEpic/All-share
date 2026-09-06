package pair

import (
	"strings"
	"testing"

	"github.com/mmc/all-share/shared/idkey"
)

// runHandshake performs a full pairing between an agent and a client, letting
// the caller substitute the code the client types.
func runHandshake(t *testing.T, agentCode, clientCode string) (agentOK, clientOK bool) {
	t.Helper()

	// --- Agent side: open a pairing window. ---
	agentID, err := idkey.Generate()
	if err != nil {
		t.Fatalf("agent identity: %v", err)
	}
	salt, err := NewSalt()
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	agentEph, err := NewEphemeral()
	if err != nil {
		t.Fatalf("agent ephemeral: %v", err)
	}
	codeID, err := DeriveCodeID(agentCode)
	if err != nil {
		t.Fatalf("agent code id: %v", err)
	}

	// --- Client side: look the window up and complete its half. ---
	clientID, err := idkey.Generate()
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}
	clientCodeID, err := DeriveCodeID(clientCode)
	if err != nil {
		t.Fatalf("client code id: %v", err)
	}
	if clientCodeID != codeID {
		// A wrong code fails at lookup, before any key material is exchanged.
		return false, false
	}
	clientEph, err := NewEphemeral()
	if err != nil {
		t.Fatalf("client ephemeral: %v", err)
	}
	clientPW, err := DerivePassword(clientCode, salt)
	if err != nil {
		t.Fatalf("client password: %v", err)
	}
	clientShared, err := clientEph.Shared(agentEph.PublicBytes())
	if err != nil {
		t.Fatalf("client ECDH: %v", err)
	}
	transcript := Transcript{
		CodeID: codeID, Salt: salt,
		AgentEPK: agentEph.PublicBytes(), ClientEPK: clientEph.PublicBytes(),
		AgentIDPub: agentID.Public(), ClientIDPub: clientID.Public(),
		DeviceName: "My Gaming PC", ClientLabel: "Chromebook",
	}
	clientMaster, err := Master(clientShared, clientPW, transcript)
	if err != nil {
		t.Fatalf("client master: %v", err)
	}
	clientTag := ConfirmTag(clientMaster, ClientRole)

	// --- Agent side: verify the client. ---
	agentPW, err := DerivePassword(agentCode, salt)
	if err != nil {
		t.Fatalf("agent password: %v", err)
	}
	agentShared, err := agentEph.Shared(clientEph.PublicBytes())
	if err != nil {
		t.Fatalf("agent ECDH: %v", err)
	}
	agentMaster, err := Master(agentShared, agentPW, transcript)
	if err != nil {
		t.Fatalf("agent master: %v", err)
	}
	agentOK = VerifyConfirm(agentMaster, ClientRole, clientTag)

	// --- Client side: verify the agent. ---
	agentTag := ConfirmTag(agentMaster, AgentRole)
	clientOK = VerifyConfirm(clientMaster, AgentRole, agentTag)
	return agentOK, clientOK
}

func TestPairingSucceedsWithCorrectCode(t *testing.T) {
	code, err := GenerateCode()
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	agentOK, clientOK := runHandshake(t, code, code)
	if !agentOK || !clientOK {
		t.Fatalf("mutual confirmation failed: agent=%v client=%v", agentOK, clientOK)
	}
}

func TestPairingSucceedsWithUserFormattedCode(t *testing.T) {
	code, err := GenerateCode()
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	// The user retypes what is on screen, with dashes and in lower case.
	typed := strings.ToLower(FormatCode(code))
	agentOK, clientOK := runHandshake(t, code, typed)
	if !agentOK || !clientOK {
		t.Fatalf("formatted code rejected: agent=%v client=%v", agentOK, clientOK)
	}
}

func TestPairingFailsWithWrongCode(t *testing.T) {
	code, err := GenerateCode()
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	wrong, err := GenerateCode()
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if wrong == code {
		t.Skip("astronomically unlikely collision")
	}
	agentOK, clientOK := runHandshake(t, code, wrong)
	if agentOK || clientOK {
		t.Fatalf("wrong code was accepted: agent=%v client=%v", agentOK, clientOK)
	}
}

// A single mistyped character must not pair. This is the case that separates a
// real key-confirmation step from a prefix check.
func TestPairingFailsOnSingleCharacterTypo(t *testing.T) {
	code, err := GenerateCode()
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	runes := []rune(code)
	for _, c := range codeAlphabet {
		if c != runes[len(runes)-1] {
			runes[len(runes)-1] = c
			break
		}
	}
	agentOK, clientOK := runHandshake(t, code, string(runes))
	if agentOK || clientOK {
		t.Fatal("a one-character typo produced a successful pairing")
	}
}

// The transcript binds the identity keys. An attacker who relays a valid
// exchange but swaps in their own identity key must be rejected, because that
// is precisely the substitution a hostile rendezvous would attempt.
func TestPairingRejectsSwappedIdentityKey(t *testing.T) {
	code, _ := GenerateCode()
	salt, _ := NewSalt()
	codeID, _ := DeriveCodeID(code)

	agentID, _ := idkey.Generate()
	clientID, _ := idkey.Generate()
	attackerID, _ := idkey.Generate()

	agentEph, _ := NewEphemeral()
	clientEph, _ := NewEphemeral()
	pw, _ := DerivePassword(code, salt)

	honest := Transcript{
		CodeID: codeID, Salt: salt,
		AgentEPK: agentEph.PublicBytes(), ClientEPK: clientEph.PublicBytes(),
		AgentIDPub: agentID.Public(), ClientIDPub: clientID.Public(),
		DeviceName: "PC", ClientLabel: "Chromebook",
	}
	tampered := honest
	tampered.ClientIDPub = attackerID.Public()

	clientShared, _ := clientEph.Shared(agentEph.PublicBytes())
	agentShared, _ := agentEph.Shared(clientEph.PublicBytes())

	clientMaster, _ := Master(clientShared, pw, honest)
	agentMaster, _ := Master(agentShared, pw, tampered)

	if VerifyConfirm(agentMaster, ClientRole, ConfirmTag(clientMaster, ClientRole)) {
		t.Fatal("identity substitution was not detected by the transcript")
	}
}

// The salt must matter: two pairings using the same code but different salts
// must not produce interchangeable secrets.
func TestPairingSaltSeparatesSessions(t *testing.T) {
	code, _ := GenerateCode()
	s1, _ := NewSalt()
	s2, _ := NewSalt()
	pw1, err := DerivePassword(code, s1)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	pw2, err := DerivePassword(code, s2)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if string(pw1) == string(pw2) {
		t.Fatal("different salts produced the same password")
	}
}

func TestConfirmTagsDifferByRole(t *testing.T) {
	master := make([]byte, 32)
	if string(ConfirmTag(master, ClientRole)) == string(ConfirmTag(master, AgentRole)) {
		t.Fatal("client and agent confirmation tags are identical; either could be replayed as the other")
	}
}

func TestCodeGeneratorUsesFullAlphabet(t *testing.T) {
	seen := map[rune]bool{}
	for i := 0; i < 400; i++ {
		code, err := GenerateCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(code) != CodeLength {
			t.Fatalf("code length %d, want %d", len(code), CodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(codeAlphabet, r) {
				t.Fatalf("code contains %q, which is outside the alphabet", r)
			}
			seen[r] = true
		}
	}
	if len(seen) < len(codeAlphabet)-2 {
		t.Fatalf("generator only produced %d of %d alphabet characters", len(seen), len(codeAlphabet))
	}
}

func TestCanonicalizeFoldsConfusableCharacters(t *testing.T) {
	cases := map[string]string{
		"abcd-efgh-jkmn":  "ABCDEFGHJKMN",
		"O0I1L1 23456789": "00111123456789",
		"\tab cd\n":       "ABCD",
	}
	for in, want := range cases {
		if got := Canonicalize(in); got != want {
			t.Errorf("Canonicalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateCodeRejectsBadShapes(t *testing.T) {
	good, _ := GenerateCode()
	if err := ValidateCode(FormatCode(good)); err != nil {
		t.Fatalf("well-formed code rejected: %v", err)
	}
	for _, bad := range []string{"", "SHORT", strings.Repeat("A", CodeLength+1), strings.Repeat("!", CodeLength)} {
		if err := ValidateCode(bad); err == nil {
			t.Errorf("ValidateCode(%q) accepted a malformed code", bad)
		}
	}
}

func TestEphemeralRejectsMalformedPeerKey(t *testing.T) {
	e, err := NewEphemeral()
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	for _, bad := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		if _, err := e.Shared(bad); err == nil {
			t.Errorf("accepted a %d-byte peer key", len(bad))
		}
	}
	// An all-zero peer key produces a low-order point; crypto/ecdh rejects it.
	if _, err := e.Shared(make([]byte, 32)); err == nil {
		t.Error("accepted an all-zero (low order) peer key")
	}
}

func TestDerivePasswordRejectsWrongSaltLength(t *testing.T) {
	code, _ := GenerateCode()
	if _, err := DerivePassword(code, make([]byte, SaltBytes-1)); err == nil {
		t.Fatal("accepted a short salt")
	}
}

func BenchmarkDeriveCodeID(b *testing.B) {
	code, _ := GenerateCode()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DeriveCodeID(code); err != nil {
			b.Fatal(err)
		}
	}
}
