// Package turnsvc runs the relay of last resort and mints its credentials.
//
// Direct peer-to-peer is the goal: it is faster, cheaper and keeps traffic off
// the server entirely. But roughly one connection in ten cannot be made
// directly — symmetric NAT on both ends, carrier-grade NAT on mobile networks,
// or a network that blocks UDP outright — and a remote desktop that fails one
// time in ten is not a product. TURN is what turns "usually works" into
// "always works".
//
// Credentials are ephemeral and derived by HMAC from a shared secret
// (the long-standing REST-API-for-TURN scheme), so no account exists to leak
// and a captured credential stops working within hours.
package turnsvc

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/turn/v5"

	"github.com/mmc/all-share/shared/protocol"
)

// Config configures the embedded relay.
type Config struct {
	// PublicIP is the address remote peers can reach. Required: a TURN server
	// that advertises a private address allocates unreachable candidates.
	PublicIP string
	// ListenAddr is the UDP address to bind, for example ":3478".
	ListenAddr string
	// Realm is the TURN realm.
	Realm string
	// Secret keys the ephemeral-credential HMAC.
	Secret []byte
	// TTL is how long a minted credential remains valid.
	TTL time.Duration
	// ExternalURLs are additional STUN/TURN servers to advertise, for
	// deployments that prefer a managed relay over the embedded one.
	ExternalURLs []protocol.ICEServer
	// STUNOnly advertises public STUN servers but no relay. Cheapest to run,
	// but leaves the hardest networks unable to connect.
	STUNOnly bool
	Log      *slog.Logger
}

// Service is a running relay plus its credential minter.
type Service struct {
	cfg    Config
	server *turn.Server
	log    *slog.Logger
}

// DefaultSTUNServers are used when no relay is configured. They perform address
// discovery only and never carry media.
var DefaultSTUNServers = []string{
	"stun:stun.l.google.com:19302",
	"stun:stun.cloudflare.com:3478",
}

// New starts the embedded relay if one is configured.
func New(cfg Config) (*Service, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 12 * time.Hour
	}
	if cfg.Realm == "" {
		cfg.Realm = "allshare"
	}
	s := &Service{cfg: cfg, log: cfg.Log}

	if cfg.STUNOnly || cfg.ListenAddr == "" || len(cfg.Secret) == 0 {
		if !cfg.STUNOnly && cfg.ListenAddr != "" {
			return nil, fmt.Errorf("turnsvc: a relay address was given without a shared secret")
		}
		cfg.Log.Info("relay disabled; advertising STUN only",
			"note", "connections that cannot be made directly will fail on restrictive networks")
		return s, nil
	}
	if cfg.PublicIP == "" {
		return nil, fmt.Errorf("turnsvc: a relay needs a public IP address to advertise")
	}
	ip := net.ParseIP(cfg.PublicIP)
	if ip == nil {
		return nil, fmt.Errorf("turnsvc: %q is not a valid IP address", cfg.PublicIP)
	}

	conn, err := net.ListenPacket("udp4", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("turnsvc: listen on %s: %w", cfg.ListenAddr, err)
	}
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			key, ok := authenticate(cfg.Secret, cfg.Realm, ra.Username)
			return ra.Username, key, ok
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: conn,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: ip,
				Address:      "0.0.0.0",
			},
		}},
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("turnsvc: start relay: %w", err)
	}
	s.server = srv
	cfg.Log.Info("relay listening", "addr", cfg.ListenAddr, "public", cfg.PublicIP, "realm", cfg.Realm)
	return s, nil
}

// Close stops the relay.
func (s *Service) Close() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// ICEServers mints a fresh credential set for one peer.
func (s *Service) ICEServers() ([]protocol.ICEServer, int64) {
	out := make([]protocol.ICEServer, 0, 4)
	out = append(out, protocol.ICEServer{URLs: DefaultSTUNServers})

	var expires int64
	if s.server != nil && len(s.cfg.Secret) > 0 && s.cfg.PublicIP != "" {
		username, credential, exp := mintCredential(s.cfg.Secret, s.cfg.TTL)
		expires = exp
		port := portOf(s.cfg.ListenAddr, 3478)
		host := net.JoinHostPort(s.cfg.PublicIP, strconv.Itoa(port))
		out = append(out, protocol.ICEServer{
			URLs: []string{
				"stun:" + host,
				"turn:" + host + "?transport=udp",
			},
			Username:   username,
			Credential: credential,
		})
	}
	out = append(out, s.cfg.ExternalURLs...)
	return out, expires
}

// mintCredential produces a time-limited TURN credential.
//
// The username is "<unix expiry>" and the password is the base64 HMAC-SHA1 of
// that username under the shared secret — the scheme every mainstream TURN
// implementation understands, so the same secret works with the embedded relay
// or with coturn.
func mintCredential(secret []byte, ttl time.Duration) (username, credential string, expiresAt int64) {
	expiresAt = time.Now().Add(ttl).Unix()
	username = strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential, expiresAt
}

// authenticate validates a TURN username and derives its expected key.
func authenticate(secret []byte, realm, username string) ([]byte, bool) {
	// The username encodes its own expiry; anything stale or malformed is
	// refused before any key derivation happens.
	cutoff := username
	if idx := strings.IndexByte(username, ':'); idx >= 0 {
		cutoff = username[:idx]
	}
	expiry, err := strconv.ParseInt(cutoff, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return nil, false
	}
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return turn.GenerateAuthKey(username, realm, password), true
}

func portOf(addr string, fallback int) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fallback
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 {
		return fallback
	}
	return p
}
