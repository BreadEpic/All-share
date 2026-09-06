// Package signal implements the ALL SHARE rendezvous: presence, pairing
// brokerage, session introduction and signalling relay.
//
// The design goal is to be worth very little to an attacker. The server sees
// device names, public keys and opaque signalling blobs. It never holds a
// pairing code, a session key or a video frame, and every SDP it relays is
// signed end to end by a key the peers pinned during pairing. Compromising this
// server buys denial of service and a device inventory — not access.
package signal

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

// Connection tuning.
const (
	// maxMessageBytes bounds one rendezvous frame. SDP with many ICE candidates
	// is a few kilobytes; 128 KiB is generous and still caps memory per peer.
	maxMessageBytes = 128 * 1024

	// sendQueue is how many frames may await a slow peer before it is dropped.
	// Signalling is bursty but tiny, so a peer this far behind is broken.
	sendQueue = 64

	authTimeout     = 15 * time.Second
	pingInterval    = 25 * time.Second
	pongTimeout     = 20 * time.Second
	writeTimeout    = 10 * time.Second
	idleReadTimeout = 90 * time.Second

	// flushGrace is how long a closing connection is given to deliver frames
	// that are already queued — in practice, a final error message.
	flushGrace = 2 * time.Second
)

var connSeq atomic.Uint64

// Conn is one authenticated rendezvous peer.
type Conn struct {
	id   uint64
	hub  *Hub
	ws   *websocket.Conn
	log  *slog.Logger
	addr string

	role     protocol.Role
	deviceID string
	pub      idkey.PublicKey

	send      chan []byte
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu       sync.Mutex
	busy     bool
	sessions map[string]struct{}

	lastPong atomic.Int64
}

// Serve upgrades an HTTP request and runs one rendezvous connection to
// completion. It returns only when the connection is finished.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r, h.trustProxy)
	if !h.connLimit.Allow(addr) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin checking is intentionally disabled.
		//
		// The Chromebook client is opened from a local file, so its Origin
		// header is literally "null" and no pattern list can express that. The
		// usual reason to check Origin — stopping a hostile page from riding
		// ambient credentials — does not apply here: this server has no cookies
		// and no ambient authority at all. Every connection must prove
		// possession of an Ed25519 private key before it can do anything, so a
		// hostile page that opens a socket gets an unauthenticated connection
		// that is closed within fifteen seconds.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		h.log.Debug("websocket upgrade failed", "addr", addr, "err", err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)

	ctx, cancel := context.WithCancel(r.Context())
	c := &Conn{
		id:       connSeq.Add(1),
		hub:      h,
		ws:       ws,
		addr:     addr,
		send:     make(chan []byte, sendQueue),
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]struct{}{},
	}
	c.log = h.log.With("conn", c.id, "addr", addr)
	c.lastPong.Store(time.Now().UnixNano())

	defer c.close(websocket.StatusNormalClosure, "")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writePump() }()
	go func() { defer wg.Done(); c.keepalive() }()

	c.readPump()
	// Give the write pump a moment to flush anything already queued. A failed
	// handshake queues an explanatory error immediately before returning, and
	// cancelling here without draining would leave the user staring at a bare
	// disconnect instead of a message telling them what went wrong.
	c.drain(flushGrace)
	cancel()
	wg.Wait()
	h.detach(c)
}

func (c *Conn) readPump() {
	// The handshake gets its own deadline so an unauthenticated socket cannot
	// linger and consume a slot.
	authCtx, cancelAuth := context.WithTimeout(c.ctx, authTimeout)
	defer cancelAuth()
	if err := c.handshake(authCtx); err != nil {
		c.log.Debug("handshake failed", "err", err)
		return
	}

	for {
		readCtx, cancel := context.WithTimeout(c.ctx, idleReadTimeout)
		typ, data, err := c.ws.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			c.fail(protocol.ErrBadRequest, "expected a text frame")
			return
		}
		if !c.hub.msgLimit.Allow(c.deviceID) {
			c.fail(protocol.ErrRateLimited, "slow down")
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.fail(protocol.ErrBadRequest, "message was not valid JSON")
			continue
		}
		if err := c.hub.dispatch(c, env); err != nil {
			c.log.Debug("dispatch error", "type", env.Type, "err", err)
		}
	}
}

// handshake runs hello → challenge → auth, proving the peer holds the private
// key matching the device ID it claims.
func (c *Conn) handshake(ctx context.Context) error {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("handshake frame was not text")
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("hello was not valid JSON: %w", err)
	}
	if env.Type != protocol.MsgHello {
		return fmt.Errorf("expected %q, got %q", protocol.MsgHello, env.Type)
	}
	var hello protocol.RVHello
	if err := json.Unmarshal(env.Body, &hello); err != nil {
		return fmt.Errorf("malformed hello: %w", err)
	}
	if hello.Version != protocol.SignalVersion {
		c.fail(protocol.ErrVersion, "This version of ALL SHARE is too old to talk to the server. Please update.")
		return fmt.Errorf("protocol version %d, want %d", hello.Version, protocol.SignalVersion)
	}
	if hello.Role != protocol.RoleAgent && hello.Role != protocol.RoleClient {
		return fmt.Errorf("unknown role %q", hello.Role)
	}
	pub, err := idkey.ParsePublic(hello.DeviceID)
	if err != nil {
		c.fail(protocol.ErrBadRequest, "That device identity is not valid.")
		return err
	}

	// Rate-limit *before* doing signature work, keyed on both the source
	// address and the claimed identity, so neither axis can be used alone to
	// force expensive verifications.
	if !c.hub.authLimit.Allow(c.addr) || !c.hub.authLimit.Allow(hello.DeviceID) {
		c.fail(protocol.ErrRateLimited, "Too many attempts. Please wait a moment.")
		return errors.New("auth rate limited")
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	nonceB64 := idkey.EncodeB64(nonce)
	c.writeMsg(protocol.MsgChallenge, protocol.Challenge{
		Nonce:     nonceB64,
		ServerID:  c.hub.serverID,
		ServerNow: time.Now().UnixMilli(),
	})

	typ, data, err = c.ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("auth frame was not text")
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("auth was not valid JSON: %w", err)
	}
	if env.Type != protocol.MsgAuth {
		return fmt.Errorf("expected %q, got %q", protocol.MsgAuth, env.Type)
	}
	var auth protocol.Auth
	if err := json.Unmarshal(env.Body, &auth); err != nil {
		return fmt.Errorf("malformed auth: %w", err)
	}
	if auth.Nonce != nonceB64 || auth.DeviceID != hello.DeviceID {
		c.fail(protocol.ErrUnauthorized, "Sign-in failed. Please try again.")
		return errors.New("auth did not match the challenge")
	}
	sig, err := idkey.DecodeB64(auth.Signature)
	if err != nil {
		c.fail(protocol.ErrUnauthorized, "Sign-in failed. Please try again.")
		return fmt.Errorf("malformed signature: %w", err)
	}
	// The signature covers the role and the server identity as well as the
	// nonce, so a challenge answered for one server or one role cannot be
	// replayed against another.
	if !pub.Verify(idkey.DomainAuth, sig, nonce, []byte(c.hub.serverID), []byte(hello.Role)) {
		c.fail(protocol.ErrUnauthorized, "Sign-in failed. Please try again.")
		return errors.New("signature did not verify")
	}

	c.role = hello.Role
	c.deviceID = hello.DeviceID
	c.pub = pub
	c.log = c.log.With("role", string(c.role), "device", pub.Fingerprint())

	if err := c.hub.attach(c); err != nil {
		c.fail(protocol.ErrInternal, "The server could not accept this connection.")
		return err
	}

	ice, expires := c.hub.iceServers()
	c.writeMsg(protocol.MsgAuthOK, protocol.AuthOK{
		SessionID:     fmt.Sprintf("rv-%d", c.id),
		ICEServers:    ice,
		TURNExpiresAt: expires,
		ServerNow:     time.Now().UnixMilli(),
		ServerVersion: c.hub.version,
	})
	c.log.Info("peer authenticated")
	return nil
}

// drain waits for the send queue to empty, up to a bounded grace period.
func (c *Conn) drain(grace time.Duration) {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if len(c.send) == 0 {
			// One more short pause so the frame in flight reaches the socket.
			time.Sleep(10 * time.Millisecond)
			return
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *Conn) writePump() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (c *Conn) keepalive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, pongTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
			c.lastPong.Store(time.Now().UnixNano())
			if c.role == protocol.RoleAgent && c.deviceID != "" {
				c.hub.reg.Touch(c.deviceID)
			}
		}
	}
}

// enqueue queues a frame, dropping the connection if the peer cannot keep up.
func (c *Conn) enqueue(raw []byte) {
	select {
	case c.send <- raw:
	case <-c.ctx.Done():
	default:
		// A peer this far behind on a channel that carries kilobytes is not
		// coming back; closing is better than growing an unbounded queue.
		c.log.Warn("send queue overflow, closing connection")
		c.cancel()
	}
}

func (c *Conn) writeMsg(msgType string, body any) {
	raw, err := encodeEnvelope(msgType, "", body)
	if err != nil {
		c.log.Error("encode outgoing message", "type", msgType, "err", err)
		return
	}
	c.enqueue(raw)
}

func (c *Conn) writeReply(msgType, ref string, body any) {
	raw, err := encodeEnvelope(msgType, ref, body)
	if err != nil {
		c.log.Error("encode outgoing reply", "type", msgType, "err", err)
		return
	}
	c.enqueue(raw)
}

func (c *Conn) fail(code protocol.ErrorCode, message string) {
	c.writeMsg(protocol.MsgError, protocol.ErrorBody{Code: code, Message: message})
}

func (c *Conn) failRef(ref string, code protocol.ErrorCode, message string) {
	c.writeReply(protocol.MsgError, ref, protocol.ErrorBody{Code: code, Message: message})
}

func (c *Conn) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.ws.Close(code, reason)
	})
}

func (c *Conn) setBusy(busy bool) {
	c.mu.Lock()
	c.busy = busy
	c.mu.Unlock()
}

func (c *Conn) isBusy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy
}

func (c *Conn) addSession(id string) {
	c.mu.Lock()
	c.sessions[id] = struct{}{}
	c.mu.Unlock()
}

func (c *Conn) dropSession(id string) {
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
}

func (c *Conn) sessionIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		out = append(out, id)
	}
	return out
}

func encodeEnvelope(msgType, ref string, body any) ([]byte, error) {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(protocol.Envelope{Type: msgType, Ref: ref, Body: raw})
}

// clientAddr resolves the peer address used for rate limiting.
//
// X-Forwarded-For is honoured only when the operator has explicitly declared
// that the server sits behind a trusted proxy. Trusting it unconditionally
// would let any caller forge a source address and escape every rate limit.
func clientAddr(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			for i := 0; i < len(fwd); i++ {
				if fwd[i] == ',' {
					return trimSpace(fwd[:i])
				}
			}
			return trimSpace(fwd)
		}
		if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
			return trimSpace(realIP)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
