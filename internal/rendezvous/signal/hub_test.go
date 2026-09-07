package signal_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mmc/all-share/internal/rendezvous/pairing"
	"github.com/mmc/all-share/internal/rendezvous/registry"
	allsignal "github.com/mmc/all-share/internal/rendezvous/signal"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/pair"
	"github.com/mmc/all-share/shared/protocol"
	"github.com/mmc/all-share/shared/rvclient"
)

type fakeICE struct{}

func (fakeICE) ICEServers() ([]protocol.ICEServer, int64) {
	return []protocol.ICEServer{{URLs: []string{"stun:stun.example:3478"}}}, time.Now().Add(time.Hour).Unix()
}

type harness struct {
	t      *testing.T
	server *httptest.Server
	hub    *allsignal.Hub
	reg    *registry.Registry
	pairs  *pairing.Store
	url    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	reg, err := registry.Open("")
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	pairs := pairing.NewStore()
	hub := allsignal.NewHub(allsignal.Config{
		Log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Registry:   reg,
		Pairs:      pairs,
		ICE:        fakeICE{},
		ServerID:   "test-server",
		Version:    "test",
		Limits:     allsignal.DefaultLimits(),
		LANKeySalt: []byte("test-lan-salt-0123456789abcdef01"),
	})
	srv := httptest.NewServer(http.HandlerFunc(hub.Serve))
	t.Cleanup(srv.Close)

	return &harness{
		t: t, server: srv, hub: hub, reg: reg, pairs: pairs,
		url: "ws" + strings.TrimPrefix(srv.URL, "http"),
	}
}

// peer wraps an rvclient with a running context and captured messages.
type peer struct {
	client   *rvclient.Client
	identity idkey.PrivateKey
	cancel   context.CancelFunc
	inbox    chan protocol.Envelope
}

func (h *harness) dial(role protocol.Role, types ...string) *peer {
	h.t.Helper()
	id, err := idkey.Generate()
	if err != nil {
		h.t.Fatalf("generate identity: %v", err)
	}
	return h.dialAs(role, id, types...)
}

func (h *harness) dialAs(role protocol.Role, id idkey.PrivateKey, types ...string) *peer {
	h.t.Helper()
	c, err := rvclient.New(rvclient.Config{
		URL:      h.url,
		Role:     role,
		Identity: id,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		h.t.Fatalf("new client: %v", err)
	}
	p := &peer{client: c, identity: id, inbox: make(chan protocol.Envelope, 64)}
	for _, ty := range types {
		ty := ty
		c.On(ty, func(env protocol.Envelope) {
			select {
			case p.inbox <- env:
			default:
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() { _ = c.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); _ = c.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("peer did not authenticate within 5s")
	return nil
}

func (p *peer) id() string { return p.identity.Public().String() }

func (p *peer) await(t *testing.T, msgType string, timeout time.Duration) protocol.Envelope {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case env := <-p.inbox:
			if env.Type == msgType {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", msgType)
		}
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------

func TestAuthenticatedPeersConnect(t *testing.T) {
	h := newHarness(t)
	agent := h.dial(protocol.RoleAgent)
	client := h.dial(protocol.RoleClient)
	if !agent.client.Connected() || !client.client.Connected() {
		t.Fatal("peers did not both reach the connected state")
	}
	if got := h.hub.Stats()["agentsOnline"]; got != 1 {
		t.Fatalf("agentsOnline = %d, want 1", got)
	}
}

func TestAgentRegistrationAppearsForPairedClientOnly(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	stranger := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)

	if err := agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name:          "My Gaming PC",
		OS:            "Windows 11",
		PairedClients: []string{client.id()},
		Wake:          protocol.WakeCapability{Method: "checkin", EstimatedSeconds: 900, Armed: true},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgListDevices, nil)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	var body protocol.DevicesBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("decode devices: %v", err)
	}
	if len(body.Devices) != 1 || body.Devices[0].Name != "My Gaming PC" || !body.Devices[0].Online {
		t.Fatalf("paired client saw %+v", body.Devices)
	}

	env, err = stranger.client.Request(ctxT(t), protocol.MsgListDevices, nil)
	if err != nil {
		t.Fatalf("stranger list: %v", err)
	}
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("decode devices: %v", err)
	}
	if len(body.Devices) != 0 {
		t.Fatalf("an unpaired client was shown %d devices; it must see none", len(body.Devices))
	}
}

func TestConnectRequiresPairing(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)

	if err := agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	_, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{Target: agent.id()})
	var rvErr *rvclient.RendezvousError
	if !errors.As(err, &rvErr) || rvErr.Code != protocol.ErrNotPaired {
		t.Fatalf("unpaired connect returned %v, want %s", err, protocol.ErrNotPaired)
	}
}

func TestConnectIntroducesSessionAndRelaysSignedSignalling(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient, protocol.MsgSignal)
	agent := h.dial(protocol.RoleAgent, protocol.MsgConnectRequest, protocol.MsgSignal)

	if err := agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{
		Target: agent.id(), Label: "Chromebook",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var ack protocol.ConnectRequest
	if err := json.Unmarshal(env.Body, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.SessionID == "" {
		t.Fatal("server did not allocate a session id")
	}

	req := agent.await(t, protocol.MsgConnectRequest, 3*time.Second)
	var creq protocol.ConnectRequest
	if err := json.Unmarshal(req.Body, &creq); err != nil {
		t.Fatalf("decode connect request: %v", err)
	}
	if creq.SessionID != ack.SessionID || creq.ClientID != client.id() || creq.Label != "Chromebook" {
		t.Fatalf("agent got %+v, want session %s from %s", creq, ack.SessionID, client.id())
	}

	// The agent signs an offer; the client must be able to verify it against the
	// agent's identity key.
	payload := protocol.SignalPayload{Kind: "offer", SDP: "v=0\r\n…", Nonce: "n1", Timestamp: time.Now().UnixMilli()}
	body, err := rvclient.SignPayload(agent.identity, creq.SessionID, agent.id(), client.id(), payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := agent.client.Send(protocol.MsgSignal, body); err != nil {
		t.Fatalf("send signal: %v", err)
	}

	got := client.await(t, protocol.MsgSignal, 3*time.Second)
	var relayed protocol.SignalBody
	if err := json.Unmarshal(got.Body, &relayed); err != nil {
		t.Fatalf("decode relayed signal: %v", err)
	}
	if relayed.From != agent.id() {
		t.Fatalf("relayed From = %q, want the agent's id", relayed.From)
	}
	verified, err := rvclient.VerifyPayload(agent.identity.Public(), creq.SessionID, agent.id(), client.id(), relayed, time.Minute)
	if err != nil {
		t.Fatalf("client could not verify the agent's signature: %v", err)
	}
	if verified.SDP != payload.SDP {
		t.Fatal("relayed SDP did not survive the round trip")
	}
}

// A rendezvous that tampers with an SDP must be caught. This is the property
// that lets the server be untrusted.
func TestTamperedSignallingFailsVerification(t *testing.T) {
	agentID, _ := idkey.Generate()
	clientID, _ := idkey.Generate()

	payload := protocol.SignalPayload{Kind: "offer", SDP: "a=fingerprint:sha-256 AA:BB", Nonce: "n", Timestamp: time.Now().UnixMilli()}
	body, err := rvclient.SignPayload(agentID, "sess", agentID.Public().String(), clientID.Public().String(), payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Rewrite the DTLS fingerprint, as a man-in-the-middle would.
	tampered := payload
	tampered.SDP = "a=fingerprint:sha-256 CC:DD"
	raw, _ := json.Marshal(tampered)
	body.Payload = idkey.EncodeB64(raw)

	if _, err := rvclient.VerifyPayload(agentID.Public(), "sess", agentID.Public().String(), clientID.Public().String(), body, time.Minute); err == nil {
		t.Fatal("a rewritten SDP passed signature verification")
	}
}

func TestSignallingRejectedForOtherSessions(t *testing.T) {
	agentID, _ := idkey.Generate()
	clientID, _ := idkey.Generate()
	payload := protocol.SignalPayload{Kind: "offer", SDP: "x", Nonce: "n", Timestamp: time.Now().UnixMilli()}
	body, _ := rvclient.SignPayload(agentID, "session-A", agentID.Public().String(), clientID.Public().String(), payload)

	if _, err := rvclient.VerifyPayload(agentID.Public(), "session-B", agentID.Public().String(), clientID.Public().String(), body, time.Minute); err == nil {
		t.Fatal("a payload signed for one session verified in another")
	}
}

func TestStaleSignallingRejected(t *testing.T) {
	agentID, _ := idkey.Generate()
	clientID, _ := idkey.Generate()
	payload := protocol.SignalPayload{Kind: "offer", SDP: "x", Nonce: "n", Timestamp: time.Now().Add(-time.Hour).UnixMilli()}
	body, _ := rvclient.SignPayload(agentID, "s", agentID.Public().String(), clientID.Public().String(), payload)

	if _, err := rvclient.VerifyPayload(agentID.Public(), "s", agentID.Public().String(), clientID.Public().String(), body, time.Minute); err == nil {
		t.Fatal("an hour-old signalling payload was accepted")
	}
}

func TestOutsiderCannotInjectIntoSession(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient, protocol.MsgSignal)
	agent := h.dial(protocol.RoleAgent, protocol.MsgConnectRequest, protocol.MsgSignal)
	outsider := h.dial(protocol.RoleClient, protocol.MsgError)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC", PairedClients: []string{client.id()}})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{Target: agent.id()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var ack protocol.ConnectRequest
	_ = json.Unmarshal(env.Body, &ack)

	_, err = outsider.client.Request(ctxT(t), protocol.MsgSignal, protocol.SignalBody{
		SessionID: ack.SessionID, Payload: "AA", Signature: "AA",
	})
	var rvErr *rvclient.RendezvousError
	if !errors.As(err, &rvErr) || rvErr.Code != protocol.ErrUnauthorized {
		t.Fatalf("outsider injection returned %v, want %s", err, protocol.ErrUnauthorized)
	}
}

func TestPairingFlowEndToEnd(t *testing.T) {
	h := newHarness(t)
	agent := h.dial(protocol.RoleAgent, protocol.MsgPairSubmit)
	client := h.dial(protocol.RoleClient, protocol.MsgPairResult)

	code, err := pair.GenerateCode()
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	codeID, err := pair.DeriveCodeID(code)
	if err != nil {
		t.Fatalf("code id: %v", err)
	}
	salt, _ := pair.NewSalt()
	agentEph, _ := pair.NewEphemeral()

	if err := agent.client.Send(protocol.MsgPairPublish, protocol.PairPublish{
		CodeID: codeID, Salt: idkey.EncodeB64(salt),
		EphemeralPub: idkey.EncodeB64(agentEph.PublicBytes()),
		IdentityPub:  agent.id(), DeviceName: "My Gaming PC",
		ExpiresAt: time.Now().Add(pair.Window).Unix(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, func() bool { return h.pairs.Open() == 1 })

	// Client looks the window up with the same code.
	env, err := client.client.Request(ctxT(t), protocol.MsgPairLookup, protocol.PairLookup{CodeID: codeID})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	var offer protocol.PairOffer
	if err := json.Unmarshal(env.Body, &offer); err != nil {
		t.Fatalf("decode offer: %v", err)
	}
	if offer.DeviceName != "My Gaming PC" || offer.DeviceID != agent.id() {
		t.Fatalf("offer = %+v", offer)
	}

	// Client completes its half.
	clientEph, _ := pair.NewEphemeral()
	agentEPK, _ := idkey.DecodeB64(offer.EphemeralPub)
	offerSalt, _ := idkey.DecodeB64(offer.Salt)
	pw, _ := pair.DerivePassword(code, offerSalt)
	sharedSecret, err := clientEph.Shared(agentEPK)
	if err != nil {
		t.Fatalf("client ECDH: %v", err)
	}
	agentPubKey, _ := idkey.ParsePublic(offer.IdentityPub)
	transcript := pair.Transcript{
		CodeID: codeID, Salt: offerSalt,
		AgentEPK: agentEPK, ClientEPK: clientEph.PublicBytes(),
		AgentIDPub: agentPubKey, ClientIDPub: client.identity.Public(),
		DeviceName: offer.DeviceName, ClientLabel: "Chromebook",
	}
	master, _ := pair.Master(sharedSecret, pw, transcript)

	if err := client.client.Send(protocol.MsgPairSubmit, protocol.PairSubmit{
		CodeID: codeID, EphemeralPub: idkey.EncodeB64(clientEph.PublicBytes()),
		IdentityPub: client.id(), ClientLabel: "Chromebook",
		Confirm: idkey.EncodeB64(pair.ConfirmTag(master, pair.ClientRole)),
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Agent verifies and answers.
	sub := agent.await(t, protocol.MsgPairSubmit, 3*time.Second)
	var got protocol.PairSubmit
	if err := json.Unmarshal(sub.Body, &got); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if sub.Ref != client.id() {
		t.Fatalf("submit ref = %q, want the client id", sub.Ref)
	}
	clientEPK, _ := idkey.DecodeB64(got.EphemeralPub)
	clientPub, _ := idkey.ParsePublic(got.IdentityPub)
	agentShared, _ := agentEph.Shared(clientEPK)
	agentTranscript := transcript
	agentTranscript.ClientEPK = clientEPK
	agentTranscript.ClientIDPub = clientPub
	agentTranscript.ClientLabel = got.ClientLabel
	agentPW, _ := pair.DerivePassword(code, salt)
	agentMaster, _ := pair.Master(agentShared, agentPW, agentTranscript)

	confirm, _ := idkey.DecodeB64(got.Confirm)
	if !pair.VerifyConfirm(agentMaster, pair.ClientRole, confirm) {
		t.Fatal("agent could not verify the client's confirmation")
	}
	if err := agent.client.SendRef(protocol.MsgPairResult, client.id(), protocol.PairResult{
		OK: true, Confirm: idkey.EncodeB64(pair.ConfirmTag(agentMaster, pair.AgentRole)),
		DeviceName: "My Gaming PC", IdentityPub: agent.id(),
	}); err != nil {
		t.Fatalf("pair result: %v", err)
	}

	res := client.await(t, protocol.MsgPairResult, 3*time.Second)
	var result protocol.PairResult
	if err := json.Unmarshal(res.Body, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.OK {
		t.Fatalf("pairing failed: %+v", result)
	}
	agentConfirm, _ := idkey.DecodeB64(result.Confirm)
	if !pair.VerifyConfirm(master, pair.AgentRole, agentConfirm) {
		t.Fatal("client could not verify the agent's confirmation")
	}
	if result.DeviceID != agent.id() {
		t.Fatalf("result device id = %q, want %q", result.DeviceID, agent.id())
	}
	waitFor(t, func() bool { return h.pairs.Open() == 0 })
}

// A pairing code is single-use. A second attempt must fail even with the right
// code, so a captured handle cannot be retried.
func TestPairingCodeIsSingleUse(t *testing.T) {
	h := newHarness(t)
	agent := h.dial(protocol.RoleAgent, protocol.MsgPairSubmit)
	client := h.dial(protocol.RoleClient)

	code, _ := pair.GenerateCode()
	codeID, _ := pair.DeriveCodeID(code)
	salt, _ := pair.NewSalt()
	eph, _ := pair.NewEphemeral()
	_ = agent.client.Send(protocol.MsgPairPublish, protocol.PairPublish{
		CodeID: codeID, Salt: idkey.EncodeB64(salt),
		EphemeralPub: idkey.EncodeB64(eph.PublicBytes()), IdentityPub: agent.id(),
		DeviceName: "PC", ExpiresAt: time.Now().Add(pair.Window).Unix(),
	})
	waitFor(t, func() bool { return h.pairs.Open() == 1 })

	if err := client.client.Send(protocol.MsgPairSubmit, protocol.PairSubmit{CodeID: codeID, Confirm: "AA"}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	agent.await(t, protocol.MsgPairSubmit, 3*time.Second)

	_, err := client.client.Request(ctxT(t), protocol.MsgPairLookup, protocol.PairLookup{CodeID: codeID})
	var rvErr *rvclient.RendezvousError
	if !errors.As(err, &rvErr) || rvErr.Code != protocol.ErrPairBadCode {
		t.Fatalf("second use returned %v, want %s", err, protocol.ErrPairBadCode)
	}
}

func TestPairLookupWithUnknownCodeIsIndistinguishable(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	_, err := client.client.Request(ctxT(t), protocol.MsgPairLookup, protocol.PairLookup{CodeID: "definitely-not-a-real-handle"})
	var rvErr *rvclient.RendezvousError
	if !errors.As(err, &rvErr) || rvErr.Code != protocol.ErrPairBadCode {
		t.Fatalf("unknown handle returned %v, want %s", err, protocol.ErrPairBadCode)
	}
}

func TestWakeReportsCheckinEstimate(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
		Wake: protocol.WakeCapability{Method: "checkin", EstimatedSeconds: 900, Armed: true},
	})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	// While the agent is online there is nothing to wake.
	env, err := client.client.Request(ctxT(t), protocol.MsgWake, protocol.WakeBody{Target: agent.id()})
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	var status protocol.WakeStatus
	_ = json.Unmarshal(env.Body, &status)
	if status.Stage != "online" {
		t.Fatalf("wake of an online PC gave stage %q, want online", status.Stage)
	}

	// Take the agent away and try again.
	agent.cancel()
	_ = agent.client.Close()
	waitFor(t, func() bool { return h.hub.Stats()["agentsOnline"] == 0 })

	env, err = client.client.Request(ctxT(t), protocol.MsgWake, protocol.WakeBody{Target: agent.id()})
	if err != nil {
		t.Fatalf("wake offline: %v", err)
	}
	_ = json.Unmarshal(env.Body, &status)
	if status.Method != "checkin" || status.EstimatedSeconds != 900 {
		t.Fatalf("offline wake gave %+v, want the check-in method with its real interval", status)
	}
	if status.Stage != "waiting" {
		t.Fatalf("stage = %q, want waiting", status.Stage)
	}
}

func TestWakeUnavailableExplainsWhy(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)
	agentID := agent.id()

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
		Wake: protocol.WakeCapability{Method: "none", Armed: false},
	})
	waitFor(t, func() bool { return h.reg.Count() == 1 })
	agent.cancel()
	_ = agent.client.Close()
	waitFor(t, func() bool { return h.hub.Stats()["agentsOnline"] == 0 })

	env, err := client.client.Request(ctxT(t), protocol.MsgWake, protocol.WakeBody{Target: agentID})
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	var status protocol.WakeStatus
	_ = json.Unmarshal(env.Body, &status)
	if status.Stage != "unavailable" || status.Message == "" {
		t.Fatalf("status = %+v; an unavailable wake must explain itself", status)
	}
	if strings.Contains(strings.ToLower(status.Message), "wol") || strings.Contains(status.Message, "ARP") {
		t.Fatalf("message %q uses jargon a normal user will not understand", status.Message)
	}
}

// A LAN neighbour must be asked to send the magic packet, and must receive the
// target's MAC addresses to do it.
func TestWakeViaLANPeer(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	sleeper := h.dial(protocol.RoleAgent)
	helper := h.dial(protocol.RoleAgent, protocol.MsgWakePeer)
	sleeperID := sleeper.id()

	_ = sleeper.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "Sleeping PC", PairedClients: []string{client.id()},
		Wake: protocol.WakeCapability{
			Method: "lanPeer", Armed: true,
			MACAddresses:   []string{"aa:bb:cc:dd:ee:ff"},
			BroadcastAddrs: []string{"192.168.1.255"},
		},
	})
	_ = helper.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "Always-on PC"})
	waitFor(t, func() bool { return h.reg.Count() == 2 })

	sleeper.cancel()
	_ = sleeper.client.Close()
	waitFor(t, func() bool { return h.hub.Stats()["agentsOnline"] == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgWake, protocol.WakeBody{Target: sleeperID})
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	var status protocol.WakeStatus
	_ = json.Unmarshal(env.Body, &status)
	if status.Method != "lanPeer" || status.Stage != "sent" {
		t.Fatalf("status = %+v, want a lanPeer wake to have been sent", status)
	}

	req := helper.await(t, protocol.MsgWakePeer, 3*time.Second)
	var wp protocol.WakePeerBody
	if err := json.Unmarshal(req.Body, &wp); err != nil {
		t.Fatalf("decode wake peer: %v", err)
	}
	if wp.Target != sleeperID || len(wp.MACAddresses) != 1 || wp.MACAddresses[0] != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("helper received %+v", wp)
	}
}

// A client must never learn another machine's MAC addresses or LAN grouping.
func TestDeviceListDoesNotLeakNetworkDetails(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
		Wake: protocol.WakeCapability{
			Method: "lanPeer", MACAddresses: []string{"aa:bb:cc:dd:ee:ff"},
			BroadcastAddrs: []string{"192.168.1.255"}, LANKey: "should-be-replaced",
		},
	})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgListDevices, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var body protocol.DevicesBody
	_ = json.Unmarshal(env.Body, &body)
	if len(body.Devices) != 1 {
		t.Fatalf("want one device, got %d", len(body.Devices))
	}
	w := body.Devices[0].Wake
	if len(w.MACAddresses) != 0 || len(w.BroadcastAddrs) != 0 || w.LANKey != "" {
		t.Fatalf("device list leaked network details: %+v", w)
	}
}

// A client-claimed LAN key must be replaced by one the server derives from the
// address it actually observes.
func TestServerDerivesLANKeyRatherThanTrustingAgent(t *testing.T) {
	h := newHarness(t)
	agent := h.dial(protocol.RoleAgent)
	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", Wake: protocol.WakeCapability{LANKey: "forged-key"},
	})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	dev, err := h.reg.Get(agent.id())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dev.Wake.LANKey == "forged-key" {
		t.Fatal("the server stored the LAN key the agent claimed")
	}
	if dev.Wake.LANKey == "" {
		t.Fatal("the server did not derive a LAN key at all")
	}
}

func TestForgetDeviceOnlyRemovesTheCaller(t *testing.T) {
	h := newHarness(t)
	alice := h.dial(protocol.RoleClient)
	bob := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent, protocol.MsgForgetDevice)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{alice.id(), bob.id()},
	})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	if _, err := alice.client.Request(ctxT(t), protocol.MsgForgetDevice, map[string]string{"target": agent.id()}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	dev, _ := h.reg.Get(agent.id())
	if dev.IsPairedWith(alice.id()) {
		t.Fatal("alice was not removed")
	}
	if !dev.IsPairedWith(bob.id()) {
		t.Fatal("alice's removal also unpaired bob")
	}
	agent.await(t, protocol.MsgForgetDevice, 3*time.Second)
}

func TestSessionEndNotifiesTheOtherSide(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent, protocol.MsgConnectRequest, protocol.MsgSessionEnd)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC", PairedClients: []string{client.id()}})
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	env, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{Target: agent.id()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var ack protocol.ConnectRequest
	_ = json.Unmarshal(env.Body, &ack)
	agent.await(t, protocol.MsgConnectRequest, 3*time.Second)

	if err := client.client.Send(protocol.MsgSessionEnd, protocol.SessionEnd{SessionID: ack.SessionID, Reason: "user disconnected"}); err != nil {
		t.Fatalf("session end: %v", err)
	}
	end := agent.await(t, protocol.MsgSessionEnd, 3*time.Second)
	var body protocol.SessionEnd
	_ = json.Unmarshal(end.Body, &body)
	if body.SessionID != ack.SessionID {
		t.Fatalf("agent was told about session %q, want %q", body.SessionID, ack.SessionID)
	}
}

// Losing the client connection must tear the session down on the agent, so a
// PC never stays "busy" forever after a Chromebook lid closes.
func TestClientDisconnectEndsSession(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent, protocol.MsgConnectRequest, protocol.MsgSessionEnd)

	_ = agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC", PairedClients: []string{client.id()}})
	waitFor(t, func() bool { return h.reg.Count() == 1 })
	if _, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{Target: agent.id()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	agent.await(t, protocol.MsgConnectRequest, 3*time.Second)

	client.cancel()
	_ = client.client.Close()
	agent.await(t, protocol.MsgSessionEnd, 5*time.Second)
	waitFor(t, func() bool { return h.hub.Stats()["sessions"] == 0 })
}

// A second connection from the same agent identity replaces the first, so a
// network blip cannot leave a zombie holding the slot.
func TestAgentReconnectReplacesStaleConnection(t *testing.T) {
	h := newHarness(t)
	id, _ := idkey.Generate()
	first := h.dialAs(protocol.RoleAgent, id)
	_ = first.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC"})
	waitFor(t, func() bool { return h.hub.Stats()["agentsOnline"] == 1 })

	second := h.dialAs(protocol.RoleAgent, id)
	_ = second.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{Name: "PC"})
	waitFor(t, func() bool { return h.hub.Stats()["agentsOnline"] == 1 })
	if !second.client.Connected() {
		t.Fatal("the newer agent connection was the one dropped")
	}
}

// TestServerStateIsRebuiltByTheAgent covers running the rendezvous somewhere
// with disposable storage — a free hosting tier, a container that is replaced
// on every deploy.
//
// Losing the server's device list must not cost the user their pairings. It
// does not, because the agent re-sends its own list on every registration and
// the PC is the authoritative copy: the server's file is a cache of what the
// agent already knows. If that ever stopped being true, the symptom would be
// users being told to pair again after a deploy they never saw, which is the
// kind of thing that gets blamed on anything but the real cause.
func TestServerStateIsRebuiltByTheAgent(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	agent := h.dial(protocol.RoleAgent)

	// The PC registers, naming the client it is paired with.
	if err := agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	if _, err := client.client.Request(ctxT(t), protocol.MsgConnect,
		protocol.ConnectBody{Target: agent.id()}); err != nil {
		t.Fatalf("a paired client could not connect: %v", err)
	}

	// Wipe every trace of the device from the server, exactly as losing the
	// disk would.
	if err := h.reg.Forget(agent.id(), client.id()); err != nil {
		t.Fatalf("forget: %v", err)
	}
	h.reg.Reset()
	if h.reg.Count() != 0 {
		t.Fatalf("the registry still holds %d devices after being reset", h.reg.Count())
	}

	// With the server empty, the client is refused — the server genuinely lost
	// the state, so this is not a test that passes for the wrong reason.
	_, err := client.client.Request(ctxT(t), protocol.MsgConnect, protocol.ConnectBody{Target: agent.id()})
	if err == nil {
		t.Fatal("the server introduced a device it no longer knew about")
	}

	// The agent re-registers, as it does on every reconnect.
	if err := agent.client.Send(protocol.MsgAgentRegister, protocol.AgentRegister{
		Name: "PC", PairedClients: []string{client.id()},
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	waitFor(t, func() bool { return h.reg.Count() == 1 })

	if _, err := client.client.Request(ctxT(t), protocol.MsgConnect,
		protocol.ConnectBody{Target: agent.id()}); err != nil {
		t.Fatalf("the pairing did not survive the server losing its storage: %v", err)
	}
}

func TestUnknownMessageTypesAreIgnored(t *testing.T) {
	h := newHarness(t)
	client := h.dial(protocol.RoleClient)
	if err := client.client.Send("someFutureMessage", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The connection must survive, so a newer peer can add messages safely.
	if _, err := client.client.Request(ctxT(t), protocol.MsgListDevices, nil); err != nil {
		t.Fatalf("connection did not survive an unknown message: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met within 5s")
}
