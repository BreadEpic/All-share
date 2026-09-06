// Package agent is the ALL SHARE PC-side orchestrator.
//
// It owns the connection to the rendezvous, answers pairing requests, accepts
// sessions from paired clients, and drives the wake machinery. It deliberately
// does not know how the screen is captured or how input is applied: those are
// interfaces, which is what lets the same orchestrator be exercised end to end
// on a machine with no desktop at all.
package agentcore

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/mmc/all-share/internal/capture"
	"github.com/mmc/all-share/internal/config"
	agentinput "github.com/mmc/all-share/internal/input"
	"github.com/mmc/all-share/internal/session"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/pair"
	"github.com/mmc/all-share/shared/protocol"
	"github.com/mmc/all-share/shared/rvclient"
)

// Version is stamped at build time.
var Version = "dev"

// signalSkew bounds how old a signalling payload may be. It is generous enough
// for a slow relay and short enough that a captured message stops being useful
// long before anyone could replay it usefully.
const signalSkew = 2 * time.Minute

// PairingWindow is one open invitation to pair, held only in memory.
type PairingWindow struct {
	Code      string
	CodeID    string
	Salt      []byte
	Ephemeral pair.Ephemeral
	ExpiresAt time.Time
}

// Options configure an Agent.
type Options struct {
	Config    *config.Config
	Identity  idkey.PrivateKey
	Provider  capture.Provider
	Injector  agentinput.Injector
	Clipboard session.Clipboard
	Wake      WakeProvider
	Log       *slog.Logger

	// URL overrides the configured rendezvous, for tests and diagnostics.
	URL string
}

// WakeProvider reports and exercises this machine's wake capability.
type WakeProvider interface {
	// Capability describes how this PC can be woken, in terms the client can
	// present honestly to a user.
	Capability() protocol.WakeCapability
	// WakePeer sends a magic packet for a neighbour on the same network.
	WakePeer(macs, broadcasts []string) error
	// StayAwake holds off sleep for a while, after this PC was woken on
	// someone's behalf.
	StayAwake(d time.Duration, reason string)
}

// Agent is the running PC-side service.
type Agent struct {
	cfg      *config.Config
	identity idkey.PrivateKey
	provider capture.Provider
	injector agentinput.Injector
	clip     session.Clipboard
	wake     WakeProvider
	log      *slog.Logger

	rv *rvclient.Client

	mu         sync.Mutex
	sessions   map[string]*session.Session
	pairing    *PairingWindow
	iceServers []webrtc.ICEServer

	// events lets a user interface observe what the agent is doing.
	listeners []chan Event
	closed    bool
}

// EventKind classifies an agent event.
type EventKind string

// Agent event kinds.
const (
	EventConnected    EventKind = "connected"
	EventDisconnected EventKind = "disconnected"
	EventPairingOpen  EventKind = "pairingOpen"
	EventPairingDone  EventKind = "pairingDone"
	EventSessionStart EventKind = "sessionStart"
	EventSessionEnd   EventKind = "sessionEnd"
	EventWakeRequest  EventKind = "wakeRequest"
	EventError        EventKind = "error"
)

// Event is something the agent did, for a tray icon or a log.
type Event struct {
	Kind    EventKind
	Message string
	Detail  string
	Code    string
	Label   string
	At      time.Time
}

// New builds an agent.
func New(opts Options) (*Agent, error) {
	if opts.Config == nil {
		return nil, errors.New("allshare/agent: no configuration")
	}
	if !opts.Identity.Valid() {
		return nil, errors.New("allshare/agent: no device identity")
	}
	if opts.Provider == nil {
		return nil, errors.New("allshare/agent: no capture provider")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	url := opts.URL
	if url == "" {
		url = opts.Config.Rendezvous
	}
	if url == "" {
		return nil, errors.New("allshare/agent: no ALL SHARE service address is configured")
	}

	a := &Agent{
		cfg:      opts.Config,
		identity: opts.Identity,
		provider: opts.Provider,
		injector: opts.Injector,
		clip:     opts.Clipboard,
		wake:     opts.Wake,
		log:      opts.Log,
		sessions: map[string]*session.Session{},
	}

	client, err := rvclient.New(rvclient.Config{
		URL:      url,
		Role:     protocol.RoleAgent,
		Identity: opts.Identity,
		Log:      opts.Log,
	})
	if err != nil {
		return nil, err
	}
	a.rv = client
	a.wireHandlers()
	return a, nil
}

// Run maintains the rendezvous connection until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("ALL SHARE agent starting",
		"version", Version, "device", a.identity.Public().Fingerprint(),
		"name", a.cfg.DeviceName, "capture", a.provider.Name())
	defer a.shutdown()
	return a.rv.Run(ctx)
}

func (a *Agent) wireHandlers() {
	a.rv.OnConnected(func(ok protocol.AuthOK) {
		a.mu.Lock()
		a.iceServers = toICEServers(ok.ICEServers)
		a.mu.Unlock()
		a.register()
		a.emit(Event{Kind: EventConnected, Message: "Connected to the ALL SHARE service."})
	})
	a.rv.OnDisconnected(func(err error) {
		a.emit(Event{Kind: EventDisconnected, Message: "Not connected to the ALL SHARE service.",
			Detail: errText(err)})
	})

	a.rv.On(protocol.MsgConnectRequest, func(env protocol.Envelope) {
		var req protocol.ConnectRequest
		if err := json.Unmarshal(env.Body, &req); err != nil {
			a.log.Warn("could not read a connection request", "err", err)
			return
		}
		go a.handleConnectRequest(req)
	})

	a.rv.On(protocol.MsgSignal, func(env protocol.Envelope) {
		var body protocol.SignalBody
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return
		}
		a.handleSignal(body)
	})

	a.rv.On(protocol.MsgSessionEnd, func(env protocol.Envelope) {
		var body protocol.SessionEnd
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return
		}
		a.endSession(body.SessionID, valueOr(body.Reason, "the client disconnected"))
	})

	a.rv.On(protocol.MsgPairSubmit, func(env protocol.Envelope) {
		var body protocol.PairSubmit
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return
		}
		go a.handlePairSubmit(env.Ref, body)
	})

	a.rv.On(protocol.MsgForgetDevice, func(env protocol.Envelope) {
		clientID := env.Ref
		if clientID == "" {
			return
		}
		if err := a.cfg.RemovePairedClient(clientID); err != nil {
			a.log.Warn("could not forget a device", "err", err)
			return
		}
		a.log.Info("a device removed itself", "client", shortID(clientID))
		a.register()
	})

	a.rv.On(protocol.MsgWakePeer, func(env protocol.Envelope) {
		var body protocol.WakePeerBody
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return
		}
		go a.handleWakePeer(body)
	})

	a.rv.On(protocol.MsgStayAwake, func(env protocol.Envelope) {
		var body protocol.StayAwakeBody
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return
		}
		if a.wake != nil && body.Seconds > 0 {
			a.wake.StayAwake(time.Duration(body.Seconds)*time.Second, body.Reason)
			a.log.Info("holding off sleep", "seconds", body.Seconds, "reason", body.Reason)
		}
	})
}

// register announces this PC and its capabilities to the rendezvous.
func (a *Agent) register() {
	reg := protocol.AgentRegister{
		Name:          a.cfg.DeviceName,
		OS:            osDescription(),
		AgentVersion:  Version,
		PairedClients: a.cfg.PairedIDs(),
		Busy:          a.sessionCount() > 0,
	}
	if a.wake != nil {
		reg.Wake = a.wake.Capability()
	} else {
		reg.Wake = protocol.WakeCapability{
			Method: "none",
			Reason: "This build of ALL SHARE cannot wake this computer.",
		}
	}
	if err := a.rv.Send(protocol.MsgAgentRegister, reg); err != nil {
		a.log.Warn("could not register with the service", "err", err)
		return
	}
	a.log.Info("registered", "name", reg.Name, "pairedDevices", len(reg.PairedClients), "wake", reg.Wake.Method)
}

// updateStatus refreshes presence without re-sending the whole registration.
func (a *Agent) updateStatus() {
	update := protocol.AgentUpdate{Busy: a.sessionCount() > 0}
	if a.wake != nil {
		capability := a.wake.Capability()
		update.Wake = &capability
	}
	if err := a.rv.Send(protocol.MsgAgentUpdate, update); err != nil {
		a.log.Debug("could not send a status update", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Pairing
// ---------------------------------------------------------------------------

// BeginPairing opens a pairing window and returns the code to show the user.
//
// The code is a password, not a token: it is fed through a slow KDF on both
// sides and used to authenticate a key exchange, so the service that brokers
// the pairing never learns anything that would let it impersonate either party.
func (a *Agent) BeginPairing() (string, time.Time, error) {
	code, err := pair.GenerateCode()
	if err != nil {
		return "", time.Time{}, err
	}
	codeID, err := pair.DeriveCodeID(code)
	if err != nil {
		return "", time.Time{}, err
	}
	salt, err := pair.NewSalt()
	if err != nil {
		return "", time.Time{}, err
	}
	ephemeral, err := pair.NewEphemeral()
	if err != nil {
		return "", time.Time{}, err
	}

	expires := time.Now().Add(pair.Window)
	window := &PairingWindow{
		Code: code, CodeID: codeID, Salt: salt,
		Ephemeral: ephemeral, ExpiresAt: expires,
	}

	a.mu.Lock()
	a.pairing = window
	a.mu.Unlock()

	if err := a.rv.Send(protocol.MsgPairPublish, protocol.PairPublish{
		CodeID:       codeID,
		Salt:         idkey.EncodeB64(salt),
		EphemeralPub: idkey.EncodeB64(ephemeral.PublicBytes()),
		IdentityPub:  a.identity.Public().String(),
		DeviceName:   a.cfg.DeviceName,
		ExpiresAt:    expires.Unix(),
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("allshare/agent: could not start pairing: %w", err)
	}

	// The window is closed when it expires even if nobody uses it, so a code
	// left on screen stops being valid on its own.
	go func() {
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()
		<-timer.C
		a.mu.Lock()
		if a.pairing == window {
			a.pairing = nil
		}
		a.mu.Unlock()
	}()

	a.emit(Event{Kind: EventPairingOpen, Message: "Ready to add a device.", Detail: pair.FormatCode(code)})
	a.log.Info("pairing window opened", "expires", expires.Format(time.Kitchen))
	return code, expires, nil
}

// CancelPairing closes an open pairing window.
func (a *Agent) CancelPairing() {
	a.mu.Lock()
	had := a.pairing != nil
	a.pairing = nil
	a.mu.Unlock()
	if had {
		_ = a.rv.Send(protocol.MsgPairRevoke, nil)
		a.log.Info("pairing window closed")
	}
}

// handlePairSubmit completes the exchange and decides whether to trust a client.
func (a *Agent) handlePairSubmit(clientRef string, submit protocol.PairSubmit) {
	a.mu.Lock()
	window := a.pairing
	a.mu.Unlock()

	reply := func(result protocol.PairResult) {
		if err := a.rv.SendRef(protocol.MsgPairResult, clientRef, result); err != nil {
			a.log.Warn("could not answer a pairing request", "err", err)
		}
	}

	if window == nil || window.CodeID != submit.CodeID || time.Now().After(window.ExpiresAt) {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairExpired})
		return
	}

	// One attempt per code, whatever the outcome. Retries would turn an
	// infeasible offline attack into an online guessing game.
	a.mu.Lock()
	a.pairing = nil
	a.mu.Unlock()

	clientEPK, err := idkey.DecodeB64(submit.EphemeralPub)
	if err != nil || len(clientEPK) != 32 {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairBadCode})
		return
	}
	clientPub, err := idkey.ParsePublic(submit.IdentityPub)
	if err != nil {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairBadCode})
		return
	}
	confirm, err := idkey.DecodeB64(submit.Confirm)
	if err != nil {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairBadCode})
		return
	}

	shared, err := window.Ephemeral.Shared(clientEPK)
	if err != nil {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairBadCode})
		return
	}
	password, err := pair.DerivePassword(window.Code, window.Salt)
	if err != nil {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrInternal})
		return
	}
	label := sanitizeLabel(submit.ClientLabel)
	transcript := pair.Transcript{
		CodeID: window.CodeID, Salt: window.Salt,
		AgentEPK: window.Ephemeral.PublicBytes(), ClientEPK: clientEPK,
		AgentIDPub: a.identity.Public(), ClientIDPub: clientPub,
		DeviceName: a.cfg.DeviceName, ClientLabel: label,
	}
	master, err := pair.Master(shared, password, transcript)
	if err != nil {
		reply(protocol.PairResult{OK: false, Code: protocol.ErrInternal})
		return
	}
	if !pair.VerifyConfirm(master, pair.ClientRole, confirm) {
		a.log.Warn("a pairing attempt failed key confirmation", "client", shortID(submit.IdentityPub))
		reply(protocol.PairResult{OK: false, Code: protocol.ErrPairBadCode})
		return
	}

	if err := a.cfg.AddPairedClient(clientPub.String(), label); err != nil {
		a.log.Error("could not save the new device", "err", err)
		reply(protocol.PairResult{OK: false, Code: protocol.ErrInternal})
		return
	}

	reply(protocol.PairResult{
		OK:          true,
		Confirm:     idkey.EncodeB64(pair.ConfirmTag(master, pair.AgentRole)),
		DeviceID:    a.identity.Public().String(),
		DeviceName:  a.cfg.DeviceName,
		IdentityPub: a.identity.Public().String(),
	})
	a.register()
	a.log.Info("paired with a new device", "label", label, "client", clientPub.Fingerprint())
	a.emit(Event{Kind: EventPairingDone, Message: "Paired with " + label, Label: label})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func (a *Agent) handleConnectRequest(req protocol.ConnectRequest) {
	// The agent checks its own pairing list rather than trusting the service's
	// routing. A compromised rendezvous can introduce anyone; it cannot make
	// this PC accept them.
	if !a.cfg.IsPaired(req.ClientID) {
		a.log.Warn("refused a connection from an unpaired device", "client", shortID(req.ClientID))
		a.sendBye(req.SessionID, req.ClientID, "This device is not paired with this PC.")
		return
	}

	if !a.cfg.AllowMultiClient {
		if existing := a.currentSession(); existing != nil {
			if existing.ClientID() == req.ClientID {
				// The same device reconnecting: replace the old session so a
				// dropped Wi-Fi connection does not lock the user out of their
				// own PC until a timeout expires.
				a.log.Info("replacing an earlier session from the same device")
				a.endSession(existing.SessionID(), "replaced by a new connection")
			} else {
				a.log.Info("refused a second device", "client", shortID(req.ClientID))
				a.sendBye(req.SessionID, req.ClientID, "This PC is already in use by another device.")
				return
			}
		}
	}

	clientID := req.ClientID
	sessionID := req.SessionID
	cfg := a.cfg

	newSession, err := session.New(context.Background(), session.Config{
		SessionID:    sessionID,
		ClientID:     clientID,
		Label:        cfg.ClientLabel(clientID),
		Provider:     a.provider,
		Injector:     a.injector,
		Clipboard:    a.clip,
		ClientCodecs: req.Codecs,
		ICEServers:   a.currentICEServers(),
		DeviceName:   cfg.DeviceName,
		AgentVersion: Version,
		HasControl:   true,
		Send: func(payload protocol.SignalPayload) error {
			return a.sendSignal(sessionID, clientID, payload)
		},
		OnClosed: func(reason string) {
			a.forgetSession(sessionID)
			a.emit(Event{Kind: EventSessionEnd, Message: "Session ended.", Detail: reason})
			a.updateStatus()
		},
		Log: a.log,
		Settings: session.Settings{
			MaxBitrate:   cfg.MaxBitrateKbps * 1000,
			MinBitrate:   cfg.MinBitrateKbps * 1000,
			StartBitrate: cfg.StartBitrateKbps * 1000,
			MaxFPS:       cfg.MaxFPS,
			Preset:       protocol.QualityPreset(cfg.Preset),
			AudioEnabled: cfg.AudioEnabled,
			ClipboardIn:  cfg.ClipboardToPC,
			ClipboardOut: cfg.ClipboardFromPC,
			LocalCursor:  cfg.LocalCursor,
		},
	})
	if err != nil {
		a.log.Error("could not start a session", "err", err)
		a.sendBye(sessionID, clientID, friendlySessionError(err))
		a.emit(Event{Kind: EventError, Message: "A device could not connect.", Detail: err.Error()})
		return
	}

	a.mu.Lock()
	a.sessions[sessionID] = newSession
	a.mu.Unlock()

	cfg.TouchClient(clientID)
	label := cfg.ClientLabel(clientID)
	if label == "" {
		label = "A paired device"
	}
	a.log.Info("session started", "session", sessionID, "client", shortID(clientID), "label", label)
	a.emit(Event{Kind: EventSessionStart, Message: label + " connected.", Label: label})
	a.updateStatus()
}

func (a *Agent) handleSignal(body protocol.SignalBody) {
	a.mu.Lock()
	current := a.sessions[body.SessionID]
	a.mu.Unlock()
	if current == nil {
		return
	}

	clientKey, err := idkey.ParsePublic(current.ClientID())
	if err != nil {
		return
	}
	// Verifying here is what makes the rendezvous untrusted: the signature
	// covers the SDP, and the SDP carries the DTLS fingerprint that keys the
	// media, so a rewritten offer or answer is refused rather than used.
	payload, err := rvclient.VerifyPayload(clientKey, body.SessionID,
		current.ClientID(), a.identity.Public().String(), body, signalSkew)
	if err != nil {
		a.log.Warn("rejected a signalling message that failed verification",
			"session", body.SessionID, "err", err)
		a.endSession(body.SessionID, "the connection could not be verified")
		return
	}
	if err := current.HandleSignal(payload); err != nil {
		a.log.Warn("could not apply a signalling message", "kind", payload.Kind, "err", err)
	}
}

func (a *Agent) sendSignal(sessionID, clientID string, payload protocol.SignalPayload) error {
	body, err := rvclient.SignPayload(a.identity, sessionID,
		a.identity.Public().String(), clientID, payload)
	if err != nil {
		return err
	}
	return a.rv.Send(protocol.MsgSignal, body)
}

func (a *Agent) sendBye(sessionID, clientID, reason string) {
	payload := protocol.SignalPayload{
		Kind: "bye", Reason: reason,
		Nonce: idkey.EncodeB64(randomBytes(9)), Timestamp: time.Now().UnixMilli(),
	}
	if err := a.sendSignal(sessionID, clientID, payload); err != nil {
		a.log.Debug("could not send a refusal", "err", err)
	}
	_ = a.rv.Send(protocol.MsgSessionEnd, protocol.SessionEnd{SessionID: sessionID, Reason: reason})
}

func (a *Agent) endSession(sessionID, reason string) {
	a.mu.Lock()
	current := a.sessions[sessionID]
	a.mu.Unlock()
	if current != nil {
		current.Close(reason)
	}
}

func (a *Agent) forgetSession(sessionID string) {
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()
}

func (a *Agent) currentSession() *session.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		return s
	}
	return nil
}

func (a *Agent) sessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// DisconnectAll ends every session immediately. This is the "give me my PC
// back" control, and it must always work.
func (a *Agent) DisconnectAll(reason string) int {
	a.mu.Lock()
	sessions := make([]*session.Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	for _, s := range sessions {
		s.Close(reason)
	}
	if len(sessions) > 0 {
		a.log.Info("disconnected every device", "count", len(sessions), "reason", reason)
	}
	return len(sessions)
}

func (a *Agent) currentICEServers() []webrtc.ICEServer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]webrtc.ICEServer(nil), a.iceServers...)
}

// ---------------------------------------------------------------------------
// Wake
// ---------------------------------------------------------------------------

func (a *Agent) handleWakePeer(body protocol.WakePeerBody) {
	if !a.cfg.AllowLANWakeHelp {
		a.log.Info("declined to wake a neighbour; helping is turned off in settings")
		return
	}
	if a.wake == nil {
		return
	}
	if err := a.wake.WakePeer(body.MACAddresses, body.BroadcastAddrs); err != nil {
		a.log.Warn("could not send a wake packet for a neighbour", "err", err)
		return
	}
	a.log.Info("sent a wake packet for a neighbour", "target", shortID(body.Target))
	a.emit(Event{Kind: EventWakeRequest, Message: "Woke another PC on this network."})
}

// ---------------------------------------------------------------------------
// Status and events
// ---------------------------------------------------------------------------

// Status describes what the agent is doing, for a tray icon or a status page.
type Status struct {
	Connected    bool
	DeviceName   string
	Fingerprint  string
	Sessions     int
	PairedCount  int
	Capture      string
	Wake         protocol.WakeCapability
	AgentVersion string
}

// Status returns a snapshot.
func (a *Agent) Status() Status {
	status := Status{
		Connected:    a.rv.Connected(),
		DeviceName:   a.cfg.DeviceName,
		Fingerprint:  a.identity.Public().Fingerprint(),
		Sessions:     a.sessionCount(),
		PairedCount:  len(a.cfg.PairedIDs()),
		Capture:      a.provider.Name(),
		AgentVersion: Version,
	}
	if a.wake != nil {
		status.Wake = a.wake.Capability()
	}
	return status
}

// Events returns a channel of agent events. The caller must drain it.
func (a *Agent) Events() <-chan Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := make(chan Event, 32)
	a.listeners = append(a.listeners, ch)
	return ch
}

func (a *Agent) emit(event Event) {
	event.At = time.Now()
	a.mu.Lock()
	listeners := append([]chan Event(nil), a.listeners...)
	a.mu.Unlock()
	for _, ch := range listeners {
		select {
		case ch <- event:
		default:
			// A listener that is not draining does not get to block the agent.
		}
	}
}

func (a *Agent) shutdown() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	listeners := a.listeners
	a.listeners = nil
	a.mu.Unlock()

	a.DisconnectAll("this PC is shutting down")
	if a.provider != nil {
		_ = a.provider.Close()
	}
	if a.injector != nil {
		_ = a.injector.Close()
	}
	for _, ch := range listeners {
		close(ch)
	}
	a.log.Info("ALL SHARE agent stopped")
}

// Config exposes the live configuration, for the tray UI and CLI.
func (a *Agent) Config() *config.Config { return a.cfg }

// Identity exposes this PC's public identity.
func (a *Agent) Identity() idkey.PublicKey { return a.identity.Public() }

// Close stops the agent.
func (a *Agent) Close() error {
	_ = a.rv.Close()
	a.shutdown()
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toICEServers(list []protocol.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(list))
	for _, server := range list {
		entry := webrtc.ICEServer{URLs: server.URLs}
		if server.Username != "" {
			entry.Username = server.Username
			entry.Credential = server.Credential
		}
		out = append(out, entry)
	}
	return out
}

// friendlySessionError turns an internal failure into something the client can
// show a user without exposing implementation detail.
func friendlySessionError(err error) string {
	if err == nil {
		return "This PC could not start a session."
	}
	message := err.Error()
	switch {
	case contains(message, "video format"):
		return "This PC cannot send video in a format that device can play."
	case contains(message, "capture"):
		return "This PC could not capture its screen."
	default:
		return "This PC could not start a session."
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func sanitizeLabel(label string) string {
	out := make([]rune, 0, 48)
	for _, r := range label {
		if r < 0x20 || r == 0x7F {
			continue
		}
		out = append(out, r)
		if len(out) >= 48 {
			break
		}
	}
	if len(out) == 0 {
		return "A device"
	}
	return string(out)
}

func osDescription() string {
	if runtime.GOOS == "windows" {
		return "Windows"
	}
	return runtime.GOOS
}

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice; if it ever did, a predictable
		// nonce would weaken replay protection, so refuse rather than continue.
		panic("allshare/agent: system randomness is unavailable: " + err.Error())
	}
	return buf
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
