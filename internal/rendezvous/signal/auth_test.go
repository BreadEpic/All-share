package signal_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

// These tests speak the rendezvous protocol by hand rather than through
// rvclient, because the point is to send things a correct client never would.

type rawConn struct {
	t  *testing.T
	ws *websocket.Conn
}

func (h *harness) raw(t *testing.T) *rawConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, h.url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &rawConn{t: t, ws: ws}
}

func (r *rawConn) send(msgType string, body any) {
	r.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("marshal: %v", err)
	}
	data, err := json.Marshal(protocol.Envelope{Type: msgType, Body: raw})
	if err != nil {
		r.t.Fatalf("marshal envelope: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ws.Write(ctx, websocket.MessageText, data); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

func (r *rawConn) recv() (protocol.Envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var env protocol.Envelope
	_, data, err := r.ws.Read(ctx)
	if err != nil {
		return env, err
	}
	return env, json.Unmarshal(data, &env)
}

// challenge runs hello and returns the server's nonce and identity.
func (r *rawConn) challenge(role protocol.Role, deviceID string) protocol.Challenge {
	r.t.Helper()
	r.send(protocol.MsgHello, protocol.RVHello{
		Version: protocol.SignalVersion, Role: role, DeviceID: deviceID, Agent: "test",
	})
	env, err := r.recv()
	if err != nil {
		r.t.Fatalf("read challenge: %v", err)
	}
	if env.Type != protocol.MsgChallenge {
		r.t.Fatalf("expected a challenge, got %q", env.Type)
	}
	var ch protocol.Challenge
	if err := json.Unmarshal(env.Body, &ch); err != nil {
		r.t.Fatalf("decode challenge: %v", err)
	}
	return ch
}

func TestAuthRejectsGarbageSignature(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	c := h.raw(t)
	ch := c.challenge(protocol.RoleClient, id.Public().String())

	c.send(protocol.MsgAuth, protocol.Auth{
		DeviceID: id.Public().String(), Nonce: ch.Nonce,
		Signature: idkey.EncodeB64(make([]byte, 64)),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertUnauthorized(t, env)
}

// Signing with one key while claiming another must fail: otherwise anyone could
// impersonate any device by naming its public key.
func TestAuthRejectsSignatureFromDifferentKey(t *testing.T) {
	h := newHarness(t)
	victim, _ := idkey.Generate()
	attacker, _ := idkey.Generate()

	c := h.raw(t)
	ch := c.challenge(protocol.RoleClient, victim.Public().String())
	nonce, _ := idkey.DecodeB64(ch.Nonce)
	sig := attacker.Sign(idkey.DomainAuth, nonce, []byte(ch.ServerID), []byte(protocol.RoleClient))

	c.send(protocol.MsgAuth, protocol.Auth{
		DeviceID: victim.Public().String(), Nonce: ch.Nonce, Signature: idkey.EncodeB64(sig),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertUnauthorized(t, env)
}

// A signature is bound to the role it was produced for, so a client's
// authentication cannot be replayed to register as an agent.
func TestAuthRejectsRoleSubstitution(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	c := h.raw(t)
	ch := c.challenge(protocol.RoleAgent, id.Public().String())
	nonce, _ := idkey.DecodeB64(ch.Nonce)

	// Sign as a client but present the signature on an agent connection.
	sig := id.Sign(idkey.DomainAuth, nonce, []byte(ch.ServerID), []byte(protocol.RoleClient))
	c.send(protocol.MsgAuth, protocol.Auth{
		DeviceID: id.Public().String(), Nonce: ch.Nonce, Signature: idkey.EncodeB64(sig),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertUnauthorized(t, env)
}

// A signature valid for another server must not authenticate here.
func TestAuthRejectsCrossServerReplay(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	c := h.raw(t)
	ch := c.challenge(protocol.RoleClient, id.Public().String())
	nonce, _ := idkey.DecodeB64(ch.Nonce)

	sig := id.Sign(idkey.DomainAuth, nonce, []byte("some-other-server"), []byte(protocol.RoleClient))
	c.send(protocol.MsgAuth, protocol.Auth{
		DeviceID: id.Public().String(), Nonce: ch.Nonce, Signature: idkey.EncodeB64(sig),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertUnauthorized(t, env)
}

// Answering a nonce the server did not issue must fail, which is what stops a
// captured (nonce, signature) pair from being replayed on a new connection.
func TestAuthRejectsForeignNonce(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	c := h.raw(t)
	_ = c.challenge(protocol.RoleClient, id.Public().String())

	foreign := make([]byte, 32)
	for i := range foreign {
		foreign[i] = byte(i)
	}
	sig := id.Sign(idkey.DomainAuth, foreign, []byte("test-server"), []byte(protocol.RoleClient))
	c.send(protocol.MsgAuth, protocol.Auth{
		DeviceID: id.Public().String(), Nonce: idkey.EncodeB64(foreign), Signature: idkey.EncodeB64(sig),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertUnauthorized(t, env)
}

func TestHelloRejectsFutureProtocolVersion(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	c := h.raw(t)
	c.send(protocol.MsgHello, protocol.RVHello{
		Version: protocol.SignalVersion + 99, Role: protocol.RoleClient,
		DeviceID: id.Public().String(),
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var body protocol.ErrorBody
	_ = json.Unmarshal(env.Body, &body)
	if env.Type != protocol.MsgError || body.Code != protocol.ErrVersion {
		t.Fatalf("got %q/%s, want a version_mismatch error", env.Type, body.Code)
	}
	if !strings.Contains(strings.ToLower(body.Message), "update") {
		t.Fatalf("version error message %q does not tell the user what to do", body.Message)
	}
}

func TestHelloRejectsMalformedDeviceID(t *testing.T) {
	h := newHarness(t)
	c := h.raw(t)
	c.send(protocol.MsgHello, protocol.RVHello{
		Version: protocol.SignalVersion, Role: protocol.RoleClient, DeviceID: "not-a-key",
	})
	env, err := c.recv()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != protocol.MsgError {
		t.Fatalf("got %q, want an error", env.Type)
	}
}

// An unauthenticated socket must not be able to do anything at all.
func TestUnauthenticatedConnectionCannotAct(t *testing.T) {
	h := newHarness(t)
	c := h.raw(t)
	c.send(protocol.MsgListDevices, map[string]string{})
	// The server treats an out-of-order message as a failed handshake and
	// closes, rather than processing it.
	if _, err := c.recv(); err == nil {
		t.Fatal("the server answered a request on an unauthenticated connection")
	}
}

// Repeated failed authentications must be throttled so an attacker cannot burn
// server CPU on signature verification.
func TestAuthIsRateLimited(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()

	limited := false
	for attempt := 0; attempt < 25 && !limited; attempt++ {
		c := h.raw(t)
		c.send(protocol.MsgHello, protocol.RVHello{
			Version: protocol.SignalVersion, Role: protocol.RoleClient, DeviceID: id.Public().String(),
		})
		env, err := c.recv()
		if err != nil {
			continue
		}
		if env.Type == protocol.MsgError {
			var body protocol.ErrorBody
			_ = json.Unmarshal(env.Body, &body)
			if body.Code == protocol.ErrRateLimited {
				limited = true
			}
		}
	}
	if !limited {
		t.Fatal("25 rapid authentication attempts were never rate limited")
	}
}

func assertUnauthorized(t *testing.T, env protocol.Envelope) {
	t.Helper()
	if env.Type != protocol.MsgError {
		t.Fatalf("got %q, want an error envelope", env.Type)
	}
	var body protocol.ErrorBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != protocol.ErrUnauthorized && body.Code != protocol.ErrRateLimited {
		t.Fatalf("error code = %s, want unauthorized", body.Code)
	}
}
