// Package idkey implements ALL SHARE device identity.
//
// Every agent and every client owns a long-lived Ed25519 keypair generated on
// first run. The public key *is* the device identity: there are no accounts, no
// passwords and no shared secrets to leak. Pairing exchanges these public keys
// over an authenticated channel, after which each side pins the other's key.
//
// Pinning is what lets the rendezvous server be untrusted. Every signalling
// payload — including the SDP, and therefore the DTLS fingerprint that keys the
// media — is signed with these keys and verified against the pinned value. A
// server that rewrites an SDP produces an invalid signature and the session is
// refused, so it can deny service but never observe or inject.
package idkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Domain separation tags. Every signature in ALL SHARE is over a tagged input
// so that a signature produced for one purpose can never be replayed as another.
const (
	DomainAuth   = "ALLSHARE-AUTH-v1"
	DomainSignal = "ALLSHARE-SIGNAL-v1"
	DomainPair   = "ALLSHARE-PAIR-v1"
)

// ErrInvalidKey reports a malformed key encoding.
var ErrInvalidKey = errors.New("allshare/idkey: invalid key")

var b64 = base64.RawURLEncoding

// PublicKey is a device's long-lived Ed25519 public key.
type PublicKey struct {
	key ed25519.PublicKey
}

// PrivateKey is a device's long-lived Ed25519 private key.
type PrivateKey struct {
	key ed25519.PrivateKey
}

// Generate creates a fresh device identity.
func Generate() (PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("allshare/idkey: generate: %w", err)
	}
	return PrivateKey{key: priv}, nil
}

// Public returns the matching public key.
func (p PrivateKey) Public() PublicKey {
	return PublicKey{key: p.key.Public().(ed25519.PublicKey)}
}

// Valid reports whether the key has been initialised.
func (p PrivateKey) Valid() bool { return len(p.key) == ed25519.PrivateKeySize }

// Sign produces a detached signature over domain-tagged content.
func (p PrivateKey) Sign(domain string, parts ...[]byte) []byte {
	return ed25519.Sign(p.key, signingInput(domain, parts...))
}

// Valid reports whether the key has been initialised.
func (k PublicKey) Valid() bool { return len(k.key) == ed25519.PublicKeySize }

// Verify checks a detached signature over domain-tagged content.
func (k PublicKey) Verify(domain string, sig []byte, parts ...[]byte) bool {
	if !k.Valid() || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(k.key, signingInput(domain, parts...), sig)
}

// Bytes returns the raw 32-byte public key.
func (k PublicKey) Bytes() []byte {
	out := make([]byte, len(k.key))
	copy(out, k.key)
	return out
}

// String is the wire form of a device ID: unpadded base64url of the raw key.
func (k PublicKey) String() string {
	if !k.Valid() {
		return ""
	}
	return b64.EncodeToString(k.key)
}

// Equal reports whether two public keys are identical.
func (k PublicKey) Equal(other PublicKey) bool {
	return k.Valid() && other.Valid() && string(k.key) == string(other.key)
}

// ParsePublic decodes a wire-form device ID.
func ParsePublic(s string) (PublicKey, error) {
	raw, err := b64.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return PublicKey{}, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(raw), ed25519.PublicKeySize)
	}
	return PublicKey{key: ed25519.PublicKey(raw)}, nil
}

var fingerprintEnc = base32.NewEncoding("ABCDEFGHJKLMNPQRSTUVWXYZ23456789").WithPadding(base32.NoPadding)

// Fingerprint renders a short, human-comparable form of the key, for example
// "K4M7-QP2X-9NRT".
//
// It exists so a user can eyeball that the PC they are connecting to is the one
// they paired with, in the same spirit as an SSH host key fingerprint. The
// alphabet omits I, O, 0 and 1 because this string gets read aloud and retyped.
func (k PublicKey) Fingerprint() string {
	if !k.Valid() {
		return ""
	}
	sum := sha256.Sum256(append([]byte("ALLSHARE-FP-v1"), k.key...))
	s := fingerprintEnc.EncodeToString(sum[:8])[:12]
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12]
}

// signingInput builds the tagged, unambiguous byte string that is actually
// signed. Each part is length-prefixed so that no two different part lists can
// ever produce the same input.
func signingInput(domain string, parts ...[]byte) []byte {
	total := len(domain) + 1
	for _, p := range parts {
		total += 4 + len(p)
	}
	buf := make([]byte, 0, total)
	buf = append(buf, domain...)
	buf = append(buf, 0)
	for _, p := range parts {
		n := uint32(len(p))
		buf = append(buf, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
		buf = append(buf, p...)
	}
	return buf
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

const keyFileHeader = "allshare-identity-v1\n"

// Save writes the private key to path with owner-only permissions.
//
// The write goes to a temporary file in the same directory and is then renamed,
// so an interrupted save can never leave a truncated identity behind — losing
// the identity would un-pair every client.
func (p PrivateKey) Save(path string) error {
	if !p.Valid() {
		return errors.New("allshare/idkey: refusing to save an empty key")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("allshare/idkey: create key directory: %w", err)
	}
	body := keyFileHeader + b64.EncodeToString(p.key) + "\n"

	tmp, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return fmt.Errorf("allshare/idkey: create temp key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/idkey: chmod key file: %w", err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/idkey: write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/idkey: sync key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("allshare/idkey: close key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("allshare/idkey: install key file: %w", err)
	}
	return nil
}

// Load reads a private key written by Save.
func Load(path string) (PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PrivateKey{}, err
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(data), keyFileHeader))
	raw, err := b64.DecodeString(text)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return PrivateKey{}, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidKey, len(raw), ed25519.PrivateKeySize)
	}
	return PrivateKey{key: ed25519.PrivateKey(raw)}, nil
}

// LoadOrCreate returns the identity at path, creating one if absent.
func LoadOrCreate(path string) (PrivateKey, bool, error) {
	key, err := Load(path)
	switch {
	case err == nil:
		return key, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return PrivateKey{}, false, err
	}
	key, err = Generate()
	if err != nil {
		return PrivateKey{}, false, err
	}
	if err := key.Save(path); err != nil {
		return PrivateKey{}, false, err
	}
	return key, true, nil
}

// EncodeB64 is the shared unpadded-base64url encoder used across the protocol.
func EncodeB64(b []byte) string { return b64.EncodeToString(b) }

// DecodeB64 is the shared unpadded-base64url decoder used across the protocol.
func DecodeB64(s string) ([]byte, error) { return b64.DecodeString(s) }
