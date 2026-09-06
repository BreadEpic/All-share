package idkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	priv, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sig := priv.Sign(DomainSignal, []byte("session"), []byte("sdp"))
	if !priv.Public().Verify(DomainSignal, sig, []byte("session"), []byte("sdp")) {
		t.Fatal("valid signature did not verify")
	}
}

func TestVerifyRejectsWrongDomain(t *testing.T) {
	priv, _ := Generate()
	sig := priv.Sign(DomainAuth, []byte("nonce"))
	if priv.Public().Verify(DomainSignal, sig, []byte("nonce")) {
		t.Fatal("a signature made for authentication verified as a signalling signature")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	sig := a.Sign(DomainSignal, []byte("x"))
	if b.Public().Verify(DomainSignal, sig, []byte("x")) {
		t.Fatal("signature verified under the wrong public key")
	}
}

// Length-prefixing each part must make the signing input unambiguous: two
// different part lists may never collide into the same signed bytes.
func TestSigningInputIsUnambiguous(t *testing.T) {
	priv, _ := Generate()
	sig := priv.Sign(DomainSignal, []byte("ab"), []byte("c"))
	if priv.Public().Verify(DomainSignal, sig, []byte("a"), []byte("bc")) {
		t.Fatal("concatenation ambiguity: (\"ab\",\"c\") and (\"a\",\"bc\") produced the same signed input")
	}
	if priv.Public().Verify(DomainSignal, sig, []byte("abc")) {
		t.Fatal("part boundaries are not covered by the signature")
	}
}

func TestVerifyRejectsMalformedSignature(t *testing.T) {
	priv, _ := Generate()
	pub := priv.Public()
	good := priv.Sign(DomainSignal, []byte("x"))

	if pub.Verify(DomainSignal, nil, []byte("x")) {
		t.Fatal("nil signature accepted")
	}
	if pub.Verify(DomainSignal, good[:len(good)-1], []byte("x")) {
		t.Fatal("truncated signature accepted")
	}
	bad := append([]byte(nil), good...)
	bad[0] ^= 0xFF
	if pub.Verify(DomainSignal, bad, []byte("x")) {
		t.Fatal("corrupted signature accepted")
	}
	var empty PublicKey
	if empty.Verify(DomainSignal, good, []byte("x")) {
		t.Fatal("uninitialised public key verified a signature")
	}
}

func TestParsePublicRoundTrip(t *testing.T) {
	priv, _ := Generate()
	pub := priv.Public()
	got, err := ParsePublic(pub.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatal("parsed key differs from the original")
	}
}

func TestParsePublicRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "!!!!", "AAAA", strings.Repeat("A", 100)} {
		if _, err := ParsePublic(bad); err == nil {
			t.Errorf("ParsePublic(%q) accepted invalid input", bad)
		}
	}
}

func TestFingerprintIsStableAndReadable(t *testing.T) {
	priv, _ := Generate()
	pub := priv.Public()
	fp := pub.Fingerprint()
	if fp != pub.Fingerprint() {
		t.Fatal("fingerprint is not deterministic")
	}
	if len(fp) != 14 || strings.Count(fp, "-") != 2 {
		t.Fatalf("fingerprint %q is not in XXXX-XXXX-XXXX form", fp)
	}
	for _, bad := range []string{"I", "O", "0", "1", "U"} {
		if strings.Contains(fp, bad) {
			t.Errorf("fingerprint %q contains the confusable character %q", fp, bad)
		}
	}
	other, _ := Generate()
	if other.Public().Fingerprint() == fp {
		t.Fatal("two distinct keys share a fingerprint")
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "identity.key")

	priv, _ := Generate()
	if err := priv.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode is %04o, want 0600", perm)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Public().Equal(priv.Public()) {
		t.Fatal("loaded identity differs from the saved one")
	}
}

func TestLoadOrCreateIsStableAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")

	first, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("first LoadOrCreate: created=%v err=%v", created, err)
	}
	second, created, err := LoadOrCreate(path)
	if err != nil || created {
		t.Fatalf("second LoadOrCreate: created=%v err=%v", created, err)
	}
	if !first.Public().Equal(second.Public()) {
		t.Fatal("identity changed across restart, which would silently unpair every client")
	}
}

func TestLoadRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(path, []byte("allshare-identity-v1\nAAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted a truncated identity file")
	}
}
