// Package pair implements ALL SHARE device pairing.
//
// The user experience is deliberately the simplest thing that can work: the PC
// shows a twelve-character code, the user types it on the Chromebook, and the
// two devices are permanently paired. What that has to survive is a rendezvous
// server that may be hostile, so the code is not a bearer token — it is the
// password input to an authenticated key exchange.
//
// The handshake:
//
//	code            12 Crockford-base32 characters ≈ 60 bits of entropy
//	codeID          PBKDF2(code, fixed salt)     — a lookup handle
//	pw              PBKDF2(code, session salt)   — the authenticating secret
//	Z               X25519(ephemeral_client, ephemeral_agent)
//	transcript      SHA-256 over every public value in the exchange
//	master          HKDF(ikm = Z ‖ pw, salt = transcript)
//	confirmations   HMAC(master, "client") and HMAC(master, "agent")
//
// Both sides verify the other's confirmation before trusting anything. An
// attacker sitting on the rendezvous learns only public keys and a transcript;
// to recover the pairing they must guess the code, and each guess costs a full
// PBKDF2 evaluation over a 2^60 space. An online attacker gets exactly one
// guess, because a failed confirmation burns the code.
//
// Why not SPAKE2 or CPace: a true PAKE removes the offline-attack term
// entirely, which is strictly better. It also requires elliptic-curve point
// arithmetic that WebCrypto does not expose, so the browser side would have to
// ship a hand-rolled or vendored curve implementation. Given a 60-bit code, a
// 210 000-iteration KDF and a three-minute single-use window, the offline term
// is already far out of reach, and not shipping bespoke curve arithmetic into
// the security-critical path is the better trade for this product.
package pair

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mmc/all-share/shared/idkey"
)

// Tunables for the pairing handshake.
const (
	// CodeLength is the number of Crockford-base32 characters in a pairing
	// code. Twelve characters is 60 bits: long enough that an offline attack
	// against the KDF is hopeless, short enough to read off a screen.
	CodeLength = 12

	// KDFIterations is the PBKDF2-HMAC-SHA256 work factor. Two derivations run
	// per pairing, so the browser pays roughly 2 × this. The value is chosen so
	// a mid-range Chromebook completes pairing in about a second.
	KDFIterations = 210_000

	// SaltBytes is the length of the per-session pairing salt.
	SaltBytes = 16

	// Window is how long a pairing code stays valid.
	Window = 3 * time.Minute

	// MaxAttempts is how many failed confirmations a code tolerates before it
	// is destroyed. It is 1: a code is a one-shot secret, and allowing retries
	// would turn a 60-bit offline problem into an online guessing game.
	MaxAttempts = 1

	fixedIDSalt   = "ALLSHARE-PAIR-ID-v1"
	hkdfInfo      = "allshare-pair-master"
	confirmClient = "client"
	confirmAgent  = "agent"
)

// Errors returned by this package.
var (
	ErrBadCode    = errors.New("allshare/pair: pairing code is not valid")
	ErrConfirm    = errors.New("allshare/pair: key confirmation failed")
	ErrExpired    = errors.New("allshare/pair: pairing code has expired")
	ErrBadKey     = errors.New("allshare/pair: malformed key material")
	ErrCodeLength = fmt.Errorf("allshare/pair: pairing code must be %d characters", CodeLength)
)

// codeAlphabet is Crockford base32 without I, L, O and U: no character can be
// confused with another when read off a screen, and U is dropped so the
// generator cannot produce an unfortunate word.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// GenerateCode returns a fresh pairing code in canonical (unformatted) form.
func GenerateCode() (string, error) {
	buf := make([]byte, CodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("allshare/pair: read randomness: %w", err)
	}
	// rand.Read gives uniform bytes; reducing mod 32 is exact because the
	// alphabet length divides 256, so there is no modulo bias here.
	out := make([]byte, CodeLength)
	for i, b := range buf {
		out[i] = codeAlphabet[b%32]
	}
	return string(out), nil
}

// FormatCode renders a code for display, in groups of four.
func FormatCode(code string) string {
	code = Canonicalize(code)
	var b strings.Builder
	for i, r := range code {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Canonicalize normalises user input: it strips separators, upper-cases, and
// folds the characters people habitually mistype (O for 0, I and L for 1).
func Canonicalize(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range strings.ToUpper(code) {
		switch r {
		case ' ', '-', '_', '\t', '\n', '\r', '.':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		case 'U':
			r = 'V'
		}
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		} else {
			// Preserve the bad character so validation can reject it rather
			// than silently pairing with a different code than the user typed.
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidateCode checks a user-entered code's shape before doing any expensive
// key derivation, so a typo fails instantly instead of after a second of KDF.
func ValidateCode(code string) error {
	code = Canonicalize(code)
	if len(code) != CodeLength {
		return ErrCodeLength
	}
	for _, r := range code {
		if !strings.ContainsRune(codeAlphabet, r) {
			return ErrBadCode
		}
	}
	return nil
}

// DeriveCodeID computes the public lookup handle for a pairing code.
//
// The handle is derived through the same slow KDF as the secret, using a fixed
// salt. That means an attacker who scrapes handles off the rendezvous cannot
// work backwards to codes any faster than attacking the pairing itself.
func DeriveCodeID(code string) (string, error) {
	if err := ValidateCode(code); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, Canonicalize(code), []byte(fixedIDSalt), KDFIterations, 16)
	if err != nil {
		return "", fmt.Errorf("allshare/pair: derive code id: %w", err)
	}
	return idkey.EncodeB64(dk), nil
}

// DerivePassword computes the authenticating secret for a pairing session.
func DerivePassword(code string, salt []byte) ([]byte, error) {
	if err := ValidateCode(code); err != nil {
		return nil, err
	}
	if len(salt) != SaltBytes {
		return nil, fmt.Errorf("allshare/pair: salt must be %d bytes, got %d", SaltBytes, len(salt))
	}
	dk, err := pbkdf2.Key(sha256.New, Canonicalize(code), append([]byte("ALLSHARE-PAIR-PW-v1"), salt...), KDFIterations, 32)
	if err != nil {
		return nil, fmt.Errorf("allshare/pair: derive password: %w", err)
	}
	return dk, nil
}

// NewSalt returns a fresh per-session pairing salt.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("allshare/pair: read randomness: %w", err)
	}
	return salt, nil
}

// Ephemeral is one side's short-lived X25519 keypair.
type Ephemeral struct {
	priv *ecdh.PrivateKey
}

// NewEphemeral generates a fresh X25519 keypair for one pairing attempt.
func NewEphemeral() (Ephemeral, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Ephemeral{}, fmt.Errorf("allshare/pair: generate ephemeral key: %w", err)
	}
	return Ephemeral{priv: priv}, nil
}

// PublicBytes returns the 32-byte X25519 public key.
func (e Ephemeral) PublicBytes() []byte { return e.priv.PublicKey().Bytes() }

// Shared performs the X25519 agreement against a peer's public key.
func (e Ephemeral) Shared(peer []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(peer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	secret, err := e.priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	return secret, nil
}

// Transcript is every public value bound into the pairing, in a fixed order.
//
// Both confirmation tags cover it, so neither side can be tricked into
// believing it paired with a different key, a different device name, or a
// different pairing session than the one it actually completed.
type Transcript struct {
	CodeID      string
	Salt        []byte
	AgentEPK    []byte
	ClientEPK   []byte
	AgentIDPub  idkey.PublicKey
	ClientIDPub idkey.PublicKey
	DeviceName  string
	ClientLabel string
}

func (t Transcript) hash() []byte {
	h := sha256.New()
	write := func(b []byte) {
		var n [4]byte
		l := uint32(len(b))
		n[0], n[1], n[2], n[3] = byte(l), byte(l>>8), byte(l>>16), byte(l>>24)
		h.Write(n[:])
		h.Write(b)
	}
	write([]byte(idkey.DomainPair))
	write([]byte(t.CodeID))
	write(t.Salt)
	write(t.AgentEPK)
	write(t.ClientEPK)
	write(t.AgentIDPub.Bytes())
	write(t.ClientIDPub.Bytes())
	write([]byte(t.DeviceName))
	write([]byte(t.ClientLabel))
	return h.Sum(nil)
}

// Master derives the pairing master secret from the ECDH output, the
// code-derived password and the transcript.
func Master(shared, password []byte, t Transcript) ([]byte, error) {
	ikm := make([]byte, 0, len(shared)+len(password))
	ikm = append(ikm, shared...)
	ikm = append(ikm, password...)
	master, err := hkdf.Key(sha256.New, ikm, t.hash(), hkdfInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("allshare/pair: derive master secret: %w", err)
	}
	return master, nil
}

// ConfirmTag computes one side's key-confirmation value. Role is "client" or
// "agent"; the tags differ so neither side can replay the other's.
func ConfirmTag(master []byte, role string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("ALLSHARE-PAIR-CONFIRM-v1"))
	mac.Write([]byte{0})
	mac.Write([]byte(role))
	return mac.Sum(nil)
}

// VerifyConfirm checks a confirmation tag in constant time.
func VerifyConfirm(master []byte, role string, got []byte) bool {
	want := ConfirmTag(master, role)
	return subtle.ConstantTimeCompare(want, got) == 1
}

// ClientRole and AgentRole name the two confirmation roles.
const (
	ClientRole = confirmClient
	AgentRole  = confirmAgent
)
