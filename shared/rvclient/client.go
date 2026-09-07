// Package rvclient is the Go side of the ALL SHARE rendezvous protocol.
//
// The Windows agent uses it to stay registered and reachable; the test suite
// uses it to drive a real server. Keeping one implementation means the code
// exercised by tests is the code that ships.
//
// Reconnection is the interesting part. A remote-access agent that gives up
// after a dropped Wi-Fi packet is useless, so this client reconnects forever
// with exponential backoff and jitter, and re-runs registration on every
// successful connect. It never spins: the floor on the backoff is a second even
// when the server is instantly refusing connections.
package rvclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

// Tuning for the rendezvous connection.
const (
	dialTimeout      = 20 * time.Second
	handshakeTimeout = 20 * time.Second
	writeTimeout     = 10 * time.Second
	readTimeout      = 90 * time.Second
	maxMessageBytes  = 128 * 1024

	backoffMin    = 1 * time.Second
	backoffMax    = 60 * time.Second
	backoffFactor = 1.7
)

// Handler receives one decoded rendezvous message.
//
// Handlers run on the client's read goroutine, so they must not block. Anything
// slow belongs on a goroutine of its own.
type Handler func(env protocol.Envelope)

// Client is a reconnecting rendezvous connection.
type Client struct {
	url      string
	role     protocol.Role
	identity idkey.PrivateKey
	log      *slog.Logger
	dialOpts *websocket.DialOptions

	mu       sync.Mutex
	ws       *websocket.Conn
	handlers map[string]Handler
	onUp     func(protocol.AuthOK)
	onDown   func(error)

	connected atomic.Bool
	reqSeq    atomic.Uint64

	waitersMu sync.Mutex
	waiters   map[string]chan protocol.Envelope

	closeOnce sync.Once
	closeCh   chan struct{}
}

// Config configures a Client.
type Config struct {
	// URL is the rendezvous WebSocket endpoint, e.g. wss://rv.example.com/rv.
	URL string
	// Role is agent or client.
	Role protocol.Role
	// Identity is this device's long-lived key.
	Identity idkey.PrivateKey
	Log      *slog.Logger
	// DialOptions allows tests to inject an HTTP client.
	DialOptions *websocket.DialOptions
}

// ValidateEndpoint rejects a rendezvous address that would carry signalling in
// the clear.
//
// Signalling is not "just metadata": it carries SDP, which lists every ICE
// candidate (your local and public addresses) and the TURN credentials minted
// for the session. Sent over ws://, all of that is readable by anyone on the
// path, and while the end-to-end signatures still stop a tamperer from
// substituting a DTLS fingerprint, confidentiality is simply gone.
//
// So wss:// is required — with one carve-out. A rendezvous self-hosted on the
// same LAN as the PCs is a legitimate deployment, and obtaining a publicly
// trusted certificate for 192.168.1.10 is not something we can reasonably
// demand. Plain ws:// is therefore accepted for loopback and private-range
// hosts only, where the traffic never leaves the local network. Everything
// reachable from the internet must be encrypted.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("allshare/rvclient: %q is not a valid address", raw)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if isLocalHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("allshare/rvclient: %q uses ws://, which is unencrypted; "+
			"use wss:// (ws:// is allowed only for addresses on your own network)", raw)
	default:
		return fmt.Errorf("allshare/rvclient: %q must start with wss://", raw)
	}
}

// isLocalHost reports whether host names a machine that cannot be reached from
// the internet: loopback, a private IPv4 range, link-local, unique-local IPv6,
// or a .local / localhost name.
func isLocalHost(host string) bool {
	if host == "" {
		return false
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// New constructs a Client. It does not connect; call Run.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("allshare/rvclient: no rendezvous URL configured")
	}
	if err := ValidateEndpoint(cfg.URL); err != nil {
		return nil, err
	}
	if !cfg.Identity.Valid() {
		return nil, errors.New("allshare/rvclient: no device identity")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Client{
		url:      cfg.URL,
		role:     cfg.Role,
		identity: cfg.Identity,
		log:      cfg.Log,
		dialOpts: cfg.DialOptions,
		handlers: map[string]Handler{},
		waiters:  map[string]chan protocol.Envelope{},
		closeCh:  make(chan struct{}),
	}, nil
}

// On registers a handler for a message type. Call before Run.
func (c *Client) On(msgType string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[msgType] = h
}

// OnConnected sets a callback fired after each successful authentication. It is
// where an agent re-sends its registration, since the server keeps no state
// about a connection that went away.
func (c *Client) OnConnected(fn func(protocol.AuthOK)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onUp = fn
}

// OnDisconnected sets a callback fired when a connection drops.
func (c *Client) OnDisconnected(fn func(error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDown = fn
}

// Connected reports whether the client currently holds an authenticated link.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run maintains the connection until ctx is cancelled or Close is called.
func (c *Client) Run(ctx context.Context) error {
	backoff := backoffMin
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closeCh:
			return nil
		default:
		}

		start := time.Now()
		err := c.session(ctx)
		c.connected.Store(false)

		c.mu.Lock()
		onDown := c.onDown
		c.mu.Unlock()
		if onDown != nil {
			onDown(err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closeCh:
			return nil
		default:
		}

		// A connection that lasted a while probably hit a transient network
		// problem, so start over from the short backoff. One that failed
		// immediately is likely a server or config problem, so back off.
		if time.Since(start) > 60*time.Second {
			backoff = backoffMin
		}
		wait := jitter(backoff)
		c.log.Info("rendezvous connection lost, retrying", "in", wait.Round(time.Millisecond), "err", err)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.closeCh:
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = time.Duration(math.Min(float64(backoff)*backoffFactor, float64(backoffMax)))
	}
}

// session runs one connection from dial to disconnect.
func (c *Client) session(ctx context.Context) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	ws, resp, err := websocket.Dial(dialCtx, c.url, c.dialOpts)
	cancelDial()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial rendezvous: %w (HTTP %d)", err, resp.StatusCode)
		}
		return fmt.Errorf("dial rendezvous: %w", err)
	}
	ws.SetReadLimit(maxMessageBytes)
	defer ws.CloseNow()

	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
		c.failAllWaiters()
	}()

	hsCtx, cancelHS := context.WithTimeout(ctx, handshakeTimeout)
	authOK, err := c.handshake(hsCtx, ws)
	cancelHS()
	if err != nil {
		return err
	}
	c.connected.Store(true)
	c.log.Info("rendezvous connected", "server", authOK.ServerVersion, "iceServers", len(authOK.ICEServers))

	c.mu.Lock()
	onUp := c.onUp
	c.mu.Unlock()
	if onUp != nil {
		onUp(authOK)
	}

	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		typ, data, err := ws.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.log.Warn("rendezvous sent a malformed message", "err", err)
			continue
		}
		if c.deliverToWaiter(env) {
			continue
		}
		c.mu.Lock()
		h := c.handlers[env.Type]
		c.mu.Unlock()
		if h != nil {
			h(env)
		}
	}
}

func (c *Client) handshake(ctx context.Context, ws *websocket.Conn) (protocol.AuthOK, error) {
	var zero protocol.AuthOK
	pub := c.identity.Public().String()

	if err := writeEnvelope(ctx, ws, protocol.MsgHello, "", protocol.RVHello{
		Version:  protocol.SignalVersion,
		Role:     c.role,
		DeviceID: pub,
		Agent:    "allshare-go",
	}); err != nil {
		return zero, fmt.Errorf("send hello: %w", err)
	}

	env, err := readEnvelope(ctx, ws)
	if err != nil {
		return zero, fmt.Errorf("read challenge: %w", err)
	}
	if env.Type == protocol.MsgError {
		return zero, errorFrom(env)
	}
	if env.Type != protocol.MsgChallenge {
		return zero, fmt.Errorf("expected a challenge, got %q", env.Type)
	}
	var ch protocol.Challenge
	if err := json.Unmarshal(env.Body, &ch); err != nil {
		return zero, fmt.Errorf("malformed challenge: %w", err)
	}
	nonce, err := idkey.DecodeB64(ch.Nonce)
	if err != nil {
		return zero, fmt.Errorf("malformed challenge nonce: %w", err)
	}

	sig := c.identity.Sign(idkey.DomainAuth, nonce, []byte(ch.ServerID), []byte(c.role))
	if err := writeEnvelope(ctx, ws, protocol.MsgAuth, "", protocol.Auth{
		DeviceID:  pub,
		Nonce:     ch.Nonce,
		Signature: idkey.EncodeB64(sig),
	}); err != nil {
		return zero, fmt.Errorf("send auth: %w", err)
	}

	env, err = readEnvelope(ctx, ws)
	if err != nil {
		return zero, fmt.Errorf("read auth result: %w", err)
	}
	if env.Type == protocol.MsgError {
		return zero, errorFrom(env)
	}
	if env.Type != protocol.MsgAuthOK {
		return zero, fmt.Errorf("expected authentication to succeed, got %q", env.Type)
	}
	var ok protocol.AuthOK
	if err := json.Unmarshal(env.Body, &ok); err != nil {
		return zero, fmt.Errorf("malformed auth result: %w", err)
	}
	return ok, nil
}

// Send transmits a message without waiting for a reply.
func (c *Client) Send(msgType string, body any) error {
	return c.SendRef(msgType, "", body)
}

// SendRef transmits a message carrying a correlation reference.
func (c *Client) SendRef(msgType, ref string, body any) error {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return errors.New("allshare/rvclient: not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return writeEnvelope(ctx, ws, msgType, ref, body)
}

// Request sends a message and waits for a reply carrying the same reference.
//
// The server answers a request either with the expected type or with an error
// envelope; both resolve the wait, so a caller never hangs on a rejection.
func (c *Client) Request(ctx context.Context, msgType string, body any) (protocol.Envelope, error) {
	ref := fmt.Sprintf("r%d", c.reqSeq.Add(1))
	ch := make(chan protocol.Envelope, 1)

	c.waitersMu.Lock()
	c.waiters[ref] = ch
	c.waitersMu.Unlock()
	defer func() {
		c.waitersMu.Lock()
		delete(c.waiters, ref)
		c.waitersMu.Unlock()
	}()

	if err := c.SendRef(msgType, ref, body); err != nil {
		return protocol.Envelope{}, err
	}
	select {
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	case env := <-ch:
		if env.Type == protocol.MsgError {
			return env, errorFrom(env)
		}
		if env.Type == "" {
			return env, errors.New("allshare/rvclient: connection closed while waiting for a reply")
		}
		return env, nil
	}
}

func (c *Client) deliverToWaiter(env protocol.Envelope) bool {
	if env.Ref == "" {
		return false
	}
	c.waitersMu.Lock()
	ch, ok := c.waiters[env.Ref]
	if ok {
		delete(c.waiters, env.Ref)
	}
	c.waitersMu.Unlock()
	if !ok {
		return false
	}
	ch <- env
	return true
}

func (c *Client) failAllWaiters() {
	c.waitersMu.Lock()
	waiters := c.waiters
	c.waiters = map[string]chan protocol.Envelope{}
	c.waitersMu.Unlock()
	for _, ch := range waiters {
		ch <- protocol.Envelope{}
	}
}

// Close stops the client and its reconnect loop.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.closeCh) })
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws != nil {
		return ws.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Signalling helpers
// ---------------------------------------------------------------------------

// SignError describes a rejected end-to-end signalling payload.
type SignError struct{ Reason string }

func (e *SignError) Error() string { return "allshare/rvclient: " + e.Reason }

// SignPayload wraps a signalling payload in an end-to-end signature.
//
// The signature covers the session, both device identities and the payload
// bytes, so a relayed message cannot be replayed into a different session or
// re-aimed at a different peer.
func SignPayload(identity idkey.PrivateKey, sessionID, selfID, peerID string, payload protocol.SignalPayload) (protocol.SignalBody, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.SignalBody{}, err
	}
	sig := identity.Sign(idkey.DomainSignal,
		[]byte(sessionID), []byte(selfID), []byte(peerID), raw)
	return protocol.SignalBody{
		SessionID: sessionID,
		Payload:   idkey.EncodeB64(raw),
		Signature: idkey.EncodeB64(sig),
	}, nil
}

// VerifyPayload checks a relayed signalling payload against a pinned key.
//
// maxSkew rejects payloads timestamped far from now, which bounds how long a
// captured message stays useful even before the per-session nonce is considered.
func VerifyPayload(peerKey idkey.PublicKey, sessionID, peerID, selfID string, body protocol.SignalBody, maxSkew time.Duration) (protocol.SignalPayload, error) {
	var payload protocol.SignalPayload
	raw, err := idkey.DecodeB64(body.Payload)
	if err != nil {
		return payload, &SignError{Reason: "signalling payload was not valid base64"}
	}
	sig, err := idkey.DecodeB64(body.Signature)
	if err != nil {
		return payload, &SignError{Reason: "signalling signature was not valid base64"}
	}
	if !peerKey.Verify(idkey.DomainSignal, sig, []byte(sessionID), []byte(peerID), []byte(selfID), raw) {
		return payload, &SignError{Reason: "signalling payload was not signed by the paired device"}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, &SignError{Reason: "signalling payload was not valid JSON"}
	}
	if maxSkew > 0 && payload.Timestamp != 0 {
		age := time.Since(time.UnixMilli(payload.Timestamp))
		if age > maxSkew || age < -maxSkew {
			return payload, &SignError{Reason: "signalling payload is too old to be trusted"}
		}
	}
	return payload, nil
}

// ---------------------------------------------------------------------------
// Wire helpers
// ---------------------------------------------------------------------------

func writeEnvelope(ctx context.Context, ws *websocket.Conn, msgType, ref string, body any) error {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw = b
	}
	data, err := json.Marshal(protocol.Envelope{Type: msgType, Ref: ref, Body: raw})
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}

func readEnvelope(ctx context.Context, ws *websocket.Conn) (protocol.Envelope, error) {
	var env protocol.Envelope
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return env, err
	}
	if typ != websocket.MessageText {
		return env, errors.New("rendezvous sent a binary frame")
	}
	return env, json.Unmarshal(data, &env)
}

// RendezvousError is a structured failure reported by the server.
type RendezvousError struct {
	Code    protocol.ErrorCode
	Message string
}

func (e *RendezvousError) Error() string {
	return fmt.Sprintf("rendezvous: %s (%s)", e.Message, e.Code)
}

func errorFrom(env protocol.Envelope) error {
	var body protocol.ErrorBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return &RendezvousError{Code: protocol.ErrInternal, Message: "the server reported an unreadable error"}
	}
	return &RendezvousError{Code: body.Code, Message: body.Message}
}

func jitter(d time.Duration) time.Duration {
	// Full jitter over [d/2, d) keeps a fleet of agents from reconnecting in
	// lockstep after a server restart.
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}
