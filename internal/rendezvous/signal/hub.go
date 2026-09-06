package signal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mmc/all-share/internal/rendezvous/pairing"
	"github.com/mmc/all-share/internal/rendezvous/registry"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

// ICEProvider supplies the STUN/TURN configuration handed to peers.
type ICEProvider interface {
	// ICEServers returns servers for one peer plus the credential expiry.
	ICEServers() ([]protocol.ICEServer, int64)
}

// Limits collects the rate-limit budgets. They are generous for a human and
// hostile to a script.
type Limits struct {
	// AuthBurst/AuthPerSecond bound signature verifications.
	AuthBurst     int
	AuthPerSecond float64
	// PairBurst/PairPerSecond bound pairing lookups. These are the tightest
	// limits in the server: pairing handles are the one lookup an attacker
	// could try to enumerate.
	PairBurst     int
	PairPerSecond float64
	// ConnectBurst/ConnectPerSecond bound session attempts against an agent.
	ConnectBurst     int
	ConnectPerSecond float64
	// MsgBurst/MsgPerSecond bound overall message rate per peer.
	MsgBurst     int
	MsgPerSecond float64
	// ConnBurst/ConnPerSecond bound new sockets per source address.
	ConnBurst     int
	ConnPerSecond float64
}

// DefaultLimits returns the shipped rate limits.
func DefaultLimits() Limits {
	return Limits{
		AuthBurst: 10, AuthPerSecond: 0.5,
		PairBurst: 5, PairPerSecond: 0.1,
		ConnectBurst: 10, ConnectPerSecond: 0.5,
		MsgBurst: 200, MsgPerSecond: 50,
		ConnBurst: 20, ConnPerSecond: 1,
	}
}

// Session is one client-to-agent introduction in progress or in use.
type Session struct {
	ID       string
	ClientID string
	AgentID  string
	Created  time.Time
}

// Hub routes rendezvous traffic between clients and agents.
type Hub struct {
	log        *slog.Logger
	reg        *registry.Registry
	pairs      *pairing.Store
	ice        ICEProvider
	serverID   string
	version    string
	trustProxy bool

	authLimit    *Limiter
	pairLimit    *Limiter
	connectLimit *Limiter
	msgLimit     *Limiter
	connLimit    *Limiter

	wake       *wakeState
	wolSender  WOLSender
	lanKeySalt []byte

	mu       sync.RWMutex
	agents   map[string]*Conn
	clients  map[string][]*Conn
	sessions map[string]*Session
}

// Config configures a Hub.
type Config struct {
	Log        *slog.Logger
	Registry   *registry.Registry
	Pairs      *pairing.Store
	ICE        ICEProvider
	ServerID   string
	Version    string
	TrustProxy bool
	Limits     Limits
	// WOL, when set, lets a self-hosted server broadcast magic packets itself.
	WOL WOLSender
	// LANKeySalt keys the derivation that groups devices by local network. It
	// must be stable across restarts or LAN-peer wake stops working, and secret
	// so nobody can compute another network's key.
	LANKeySalt []byte
}

// NewHub constructs a rendezvous hub.
func NewHub(cfg Config) *Hub {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	l := cfg.Limits
	if l.AuthBurst == 0 {
		l = DefaultLimits()
	}
	return &Hub{
		log:          cfg.Log,
		reg:          cfg.Registry,
		pairs:        cfg.Pairs,
		ice:          cfg.ICE,
		serverID:     cfg.ServerID,
		version:      cfg.Version,
		trustProxy:   cfg.TrustProxy,
		authLimit:    NewLimiter(l.AuthBurst, l.AuthPerSecond),
		pairLimit:    NewLimiter(l.PairBurst, l.PairPerSecond),
		connectLimit: NewLimiter(l.ConnectBurst, l.ConnectPerSecond),
		msgLimit:     NewLimiter(l.MsgBurst, l.MsgPerSecond),
		connLimit:    NewLimiter(l.ConnBurst, l.ConnPerSecond),
		agents:       map[string]*Conn{},
		clients:      map[string][]*Conn{},
		sessions:     map[string]*Session{},
		wake:         newWakeState(),
		wolSender:    cfg.WOL,
		lanKeySalt:   cfg.LANKeySalt,
	}
}

// LANKeyFor derives the local-network grouping key for a peer.
//
// It is computed from the address the server actually observes, never from
// anything the agent claims. Two machines behind the same home router share a
// public address and therefore a key, which is exactly the set of machines that
// can deliver a magic packet to each other — and no agent can forge membership
// of a network it is not on.
func (h *Hub) LANKeyFor(addr string) string {
	if len(h.lanKeySalt) == 0 || addr == "" {
		return ""
	}
	mac := hmac.New(sha256.New, h.lanKeySalt)
	mac.Write([]byte("ALLSHARE-LANKEY-v1"))
	mac.Write([]byte{0})
	mac.Write([]byte(addr))
	return idkey.EncodeB64(mac.Sum(nil)[:16])
}

func (h *Hub) iceServers() ([]protocol.ICEServer, int64) {
	if h.ice == nil {
		return nil, 0
	}
	return h.ice.ICEServers()
}

// attach registers an authenticated connection.
func (h *Hub) attach(c *Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch c.role {
	case protocol.RoleAgent:
		// One agent identity means one machine. A second connection replaces
		// the first, which is what makes reconnect-after-network-blip work
		// instead of leaving a zombie holding the slot.
		if prev, ok := h.agents[c.deviceID]; ok && prev != c {
			prev.log.Info("replaced by a newer connection from the same agent")
			go prev.close(1000, "replaced by a newer connection")
		}
		h.agents[c.deviceID] = c
	case protocol.RoleClient:
		conns := h.clients[c.deviceID]
		if len(conns) >= 8 {
			return errors.New("too many concurrent connections for this client")
		}
		h.clients[c.deviceID] = append(conns, c)
	default:
		return fmt.Errorf("unknown role %q", c.role)
	}
	return nil
}

// detach removes a connection and notifies anyone who cared about it.
func (h *Hub) detach(c *Conn) {
	if c.deviceID == "" {
		return
	}
	h.mu.Lock()
	switch c.role {
	case protocol.RoleAgent:
		if h.agents[c.deviceID] == c {
			delete(h.agents, c.deviceID)
		}
	case protocol.RoleClient:
		conns := h.clients[c.deviceID]
		filtered := conns[:0]
		for _, x := range conns {
			if x != c {
				filtered = append(filtered, x)
			}
		}
		if len(filtered) == 0 {
			delete(h.clients, c.deviceID)
		} else {
			h.clients[c.deviceID] = filtered
		}
	}
	h.mu.Unlock()

	for _, sid := range c.sessionIDs() {
		h.endSession(sid, c.deviceID, "peer disconnected")
	}
	if c.role == protocol.RoleAgent {
		h.pairs.Revoke(c.deviceID)
		h.broadcastPresence(c.deviceID)
	}
	c.log.Info("peer disconnected")
}

// dispatch routes one decoded message.
func (h *Hub) dispatch(c *Conn, env protocol.Envelope) error {
	switch env.Type {
	case protocol.MsgPingRV:
		c.writeReply(protocol.MsgPongRV, env.Ref, map[string]int64{"now": time.Now().UnixMilli()})
		return nil

	// --- Agent messages ---
	case protocol.MsgAgentRegister:
		return h.onAgentRegister(c, env)
	case protocol.MsgAgentUpdate:
		return h.onAgentUpdate(c, env)
	case protocol.MsgPairPublish:
		return h.onPairPublish(c, env)
	case protocol.MsgPairRevoke:
		if c.role != protocol.RoleAgent {
			return h.reject(c, env, protocol.ErrUnauthorized, "Only a PC can cancel pairing.")
		}
		h.pairs.Revoke(c.deviceID)
		return nil
	case protocol.MsgPairResult:
		return h.onPairResult(c, env)

	// --- Client messages ---
	case protocol.MsgListDevices:
		return h.onListDevices(c, env)
	case protocol.MsgPairLookup:
		return h.onPairLookup(c, env)
	case protocol.MsgPairSubmit:
		return h.onPairSubmit(c, env)
	case protocol.MsgConnect:
		return h.onConnect(c, env)
	case protocol.MsgWake:
		return h.onWake(c, env)
	case protocol.MsgForgetDevice:
		return h.onForgetDevice(c, env)

	// --- Either side ---
	case protocol.MsgSignal:
		return h.onSignal(c, env)
	case protocol.MsgSessionEnd:
		return h.onSessionEnd(c, env)

	default:
		// Unknown types are ignored rather than fatal so a newer peer can add
		// messages without breaking an older server.
		c.log.Debug("ignoring unknown message type", "type", env.Type)
		return nil
	}
}

func (h *Hub) reject(c *Conn, env protocol.Envelope, code protocol.ErrorCode, msg string) error {
	c.failRef(env.Ref, code, msg)
	return fmt.Errorf("rejected %s: %s", env.Type, code)
}

// ---------------------------------------------------------------------------
// Agent handlers
// ---------------------------------------------------------------------------

func (h *Hub) onAgentRegister(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleAgent {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a PC can register.")
	}
	var reg protocol.AgentRegister
	if err := json.Unmarshal(env.Body, &reg); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That registration was not understood.")
	}
	// Overwrite any client-supplied LAN key with one derived from the address
	// we actually see, so LAN membership cannot be claimed, only observed.
	reg.Wake.LANKey = h.LANKeyFor(c.addr)
	dev, err := h.reg.Upsert(c.deviceID, reg)
	if err != nil {
		return h.reject(c, env, protocol.ErrInternal, "The server could not save this PC.")
	}
	c.setBusy(reg.Busy)
	c.log.Info("agent registered", "name", dev.Name, "paired", len(dev.PairedClients), "wake", dev.Wake.Method)
	h.broadcastPresence(c.deviceID)
	h.noteAgentOnline(c.deviceID)
	return nil
}

func (h *Hub) onAgentUpdate(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleAgent {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a PC can send status updates.")
	}
	var upd protocol.AgentUpdate
	if err := json.Unmarshal(env.Body, &upd); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That status update was not understood.")
	}
	c.setBusy(upd.Busy)
	if upd.Wake != nil {
		upd.Wake.LANKey = h.LANKeyFor(c.addr)
		h.reg.UpdateWake(c.deviceID, *upd.Wake)
	} else {
		h.reg.Touch(c.deviceID)
	}
	h.broadcastPresence(c.deviceID)
	return nil
}

func (h *Hub) onPairPublish(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleAgent {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a PC can start pairing.")
	}
	var p protocol.PairPublish
	if err := json.Unmarshal(env.Body, &p); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That pairing request was not understood.")
	}
	if _, err := h.pairs.Publish(c.deviceID, p); err != nil {
		return h.reject(c, env, protocol.ErrInternal, "Pairing could not be started right now.")
	}
	c.log.Info("pairing window opened")
	return nil
}

// onPairResult carries the agent's verdict back to the waiting client.
func (h *Hub) onPairResult(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleAgent {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a PC can answer a pairing request.")
	}
	var res protocol.PairResult
	if err := json.Unmarshal(env.Body, &res); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That pairing result was not understood.")
	}
	// env.Ref carries the client identity the agent is answering.
	target := env.Ref
	if target == "" {
		return h.reject(c, env, protocol.ErrBadRequest, "That pairing result had no recipient.")
	}
	res.DeviceID = c.deviceID
	h.sendToClient(target, protocol.MsgPairResult, "", res)
	if res.OK {
		h.pairs.Revoke(c.deviceID)
		c.log.Info("pairing completed", "client", shortID(target))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Client handlers
// ---------------------------------------------------------------------------

func (h *Hub) onListDevices(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can list PCs.")
	}
	devices := h.reg.ForClient(c.deviceID)
	out := make([]protocol.DeviceInfo, 0, len(devices))
	for _, d := range devices {
		out = append(out, h.deviceInfo(d))
	}
	c.writeReply(protocol.MsgDevices, env.Ref, protocol.DevicesBody{Devices: out})
	return nil
}

func (h *Hub) onPairLookup(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can look up a pairing code.")
	}
	if !h.pairLimit.Allow(c.addr) || !h.pairLimit.Allow(c.deviceID) {
		return h.reject(c, env, protocol.ErrRateLimited, "Too many pairing attempts. Please wait a minute and try again.")
	}
	var q protocol.PairLookup
	if err := json.Unmarshal(env.Body, &q); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That pairing code was not understood.")
	}
	w, err := h.pairs.Lookup(q.CodeID)
	if err != nil {
		// One message for "no such code", "expired" and "already used" alike:
		// distinguishing them would tell an attacker whether a guessed handle
		// exists, which is exactly the signal the slow KDF is there to deny.
		return h.reject(c, env, protocol.ErrPairBadCode,
			"That code did not work. Check the code on your PC, and make sure it has not expired.")
	}
	c.writeReply(protocol.MsgPairOffer, env.Ref, protocol.PairOffer{
		CodeID:       w.CodeID,
		Salt:         w.Salt,
		EphemeralPub: w.EphemeralPub,
		IdentityPub:  w.IdentityPub,
		DeviceName:   w.DeviceName,
		DeviceID:     w.AgentID,
	})
	return nil
}

func (h *Hub) onPairSubmit(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can complete pairing.")
	}
	if !h.pairLimit.Allow(c.addr) || !h.pairLimit.Allow(c.deviceID) {
		return h.reject(c, env, protocol.ErrRateLimited, "Too many pairing attempts. Please wait a minute and try again.")
	}
	var sub protocol.PairSubmit
	if err := json.Unmarshal(env.Body, &sub); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That pairing message was not understood.")
	}
	// Consuming here — before the agent has judged the confirmation — is
	// deliberate. It gives a pairing code exactly one attempt, so an attacker
	// cannot turn an offline problem into repeated online guesses.
	w, err := h.pairs.Consume(sub.CodeID)
	if err != nil {
		return h.reject(c, env, protocol.ErrPairBadCode,
			"That code did not work. Check the code on your PC, and make sure it has not expired.")
	}
	agent := h.agentConn(w.AgentID)
	if agent == nil {
		return h.reject(c, env, protocol.ErrDeviceOffline, "That PC went offline before pairing finished.")
	}
	sub.IdentityPub = c.deviceID // the client cannot claim someone else's key
	agent.writeReply(protocol.MsgPairSubmit, c.deviceID, sub)
	return nil
}

func (h *Hub) onConnect(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can start a session.")
	}
	var req protocol.ConnectBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That connection request was not understood.")
	}
	if !h.connectLimit.Allow(c.deviceID) {
		return h.reject(c, env, protocol.ErrRateLimited, "Too many connection attempts. Please wait a few seconds.")
	}
	dev, err := h.reg.Get(req.Target)
	if err != nil {
		return h.reject(c, env, protocol.ErrDeviceUnknown, "That PC is not set up with ALL SHARE any more.")
	}
	if !dev.IsPairedWith(c.deviceID) {
		return h.reject(c, env, protocol.ErrNotPaired, "This device is not paired with that PC.")
	}
	agent := h.agentConn(req.Target)
	if agent == nil {
		return h.reject(c, env, protocol.ErrDeviceOffline, "That PC is offline.")
	}

	sid, err := newSessionID()
	if err != nil {
		return h.reject(c, env, protocol.ErrInternal, "The server could not start a session.")
	}
	h.mu.Lock()
	h.sessions[sid] = &Session{ID: sid, ClientID: c.deviceID, AgentID: req.Target, Created: time.Now()}
	h.mu.Unlock()
	c.addSession(sid)
	agent.addSession(sid)

	if len(req.Codecs) > 64 {
		req.Codecs = req.Codecs[:64]
	}
	agent.writeMsg(protocol.MsgConnectRequest, protocol.ConnectRequest{
		SessionID: sid,
		ClientID:  c.deviceID,
		Label:     registry.SanitizeName(req.Label),
		Codecs:    req.Codecs,
		Reconnect: req.Reconnect,
	})
	c.writeReply(protocol.MsgConnectRequest, env.Ref, protocol.ConnectRequest{SessionID: sid, ClientID: c.deviceID})
	c.log.Info("session introduced", "session", sid, "agent", shortID(req.Target))
	return nil
}

func (h *Hub) onForgetDevice(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can remove a PC.")
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That request was not understood.")
	}
	// A client may only remove itself from a device's list, never anyone else.
	if err := h.reg.Forget(body.Target, c.deviceID); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return h.reject(c, env, protocol.ErrInternal, "The server could not update that PC.")
	}
	if agent := h.agentConn(body.Target); agent != nil {
		agent.writeReply(protocol.MsgForgetDevice, c.deviceID, map[string]string{"clientId": c.deviceID})
	}
	c.writeReply(protocol.MsgForgetDevice, env.Ref, map[string]bool{"ok": true})
	return nil
}

// ---------------------------------------------------------------------------
// Signalling relay
// ---------------------------------------------------------------------------

// onSignal relays one opaque signalling payload to the other party.
//
// The server checks only that the sender belongs to the session it names. It
// does not parse, validate or rewrite the payload: peers verify the attached
// Ed25519 signature themselves, which is what makes a compromised server unable
// to insert itself into a media path.
func (h *Hub) onSignal(c *Conn, env protocol.Envelope) error {
	var body protocol.SignalBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That signalling message was not understood.")
	}
	h.mu.RLock()
	sess, ok := h.sessions[body.SessionID]
	h.mu.RUnlock()
	if !ok {
		return h.reject(c, env, protocol.ErrBadRequest, "That session has already ended.")
	}

	var targetID string
	switch c.deviceID {
	case sess.ClientID:
		targetID = sess.AgentID
	case sess.AgentID:
		targetID = sess.ClientID
	default:
		return h.reject(c, env, protocol.ErrUnauthorized, "That session does not belong to this device.")
	}

	body.From = c.deviceID
	body.To = targetID
	if targetID == sess.AgentID {
		if agent := h.agentConn(targetID); agent != nil {
			agent.writeMsg(protocol.MsgSignal, body)
			return nil
		}
	} else {
		if h.sendToClient(targetID, protocol.MsgSignal, "", body) {
			return nil
		}
	}
	h.endSession(body.SessionID, "", "peer went away")
	return h.reject(c, env, protocol.ErrDeviceOffline, "The other device went offline.")
}

func (h *Hub) onSessionEnd(c *Conn, env protocol.Envelope) error {
	var body protocol.SessionEnd
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That request was not understood.")
	}
	h.endSession(body.SessionID, c.deviceID, body.Reason)
	return nil
}

// endSession tears a session down and tells the other party. requester may be
// empty when the server itself is cleaning up.
func (h *Hub) endSession(sessionID, requester, reason string) {
	h.mu.Lock()
	sess, ok := h.sessions[sessionID]
	if ok && requester != "" && requester != sess.ClientID && requester != sess.AgentID {
		h.mu.Unlock()
		return
	}
	if ok {
		delete(h.sessions, sessionID)
	}
	h.mu.Unlock()
	if !ok {
		return
	}

	notify := func(id string) {
		if id == requester {
			return
		}
		body := protocol.SessionEnd{SessionID: sessionID, Reason: reason}
		if id == sess.AgentID {
			if a := h.agentConn(id); a != nil {
				a.dropSession(sessionID)
				a.writeMsg(protocol.MsgSessionEnd, body)
			}
			return
		}
		h.forEachClient(id, func(cc *Conn) {
			cc.dropSession(sessionID)
			cc.writeMsg(protocol.MsgSessionEnd, body)
		})
	}
	notify(sess.AgentID)
	notify(sess.ClientID)
}

// ---------------------------------------------------------------------------
// Presence
// ---------------------------------------------------------------------------

func (h *Hub) deviceInfo(d *registry.Device) protocol.DeviceInfo {
	conn := h.agentConn(d.ID)
	info := protocol.DeviceInfo{
		DeviceID:     d.ID,
		Name:         d.Name,
		Online:       conn != nil,
		LastSeen:     d.LastSeen.UnixMilli(),
		AgentVersion: d.AgentVersion,
		OS:           d.OS,
		Wake:         d.Wake,
	}
	if conn != nil {
		info.Busy = conn.isBusy()
	}
	// A machine that is offline cannot have a usable wake path that depends on
	// it being reachable, so the honest answer is to describe what will actually
	// happen rather than advertising a button that cannot work.
	if !info.Online && info.Wake.Method == "modernStandby" {
		info.Wake.Method = "none"
		info.Wake.Reason = "This PC is fully powered down or disconnected from the network."
	}
	// The MAC addresses are only meaningful to a LAN peer performing the wake;
	// they are not sent to clients.
	info.Wake.MACAddresses = nil
	info.Wake.BroadcastAddrs = nil
	info.Wake.LANKey = ""
	return info
}

// broadcastPresence pushes a device's state to every paired client that is
// currently connected, so a PC coming online updates the UI immediately instead
// of on the next poll.
func (h *Hub) broadcastPresence(deviceID string) {
	dev, err := h.reg.Get(deviceID)
	if err != nil {
		return
	}
	info := h.deviceInfo(dev)
	for _, clientID := range dev.PairedClients {
		h.sendToClient(clientID, protocol.MsgDeviceUpdate, "", info)
	}
}

// ---------------------------------------------------------------------------
// Connection lookup
// ---------------------------------------------------------------------------

func (h *Hub) agentConn(deviceID string) *Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[deviceID]
}

func (h *Hub) forEachClient(clientID string, fn func(*Conn)) {
	h.mu.RLock()
	conns := append([]*Conn(nil), h.clients[clientID]...)
	h.mu.RUnlock()
	for _, c := range conns {
		fn(c)
	}
}

func (h *Hub) sendToClient(clientID, msgType, ref string, body any) bool {
	raw, err := encodeEnvelope(msgType, ref, body)
	if err != nil {
		h.log.Error("encode message for client", "type", msgType, "err", err)
		return false
	}
	sent := false
	h.forEachClient(clientID, func(c *Conn) {
		c.enqueue(raw)
		sent = true
	})
	return sent
}

// Stats reports live hub counters for the health endpoint.
func (h *Hub) Stats() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return map[string]int{
		"agentsOnline":  len(h.agents),
		"clientsOnline": len(h.clients),
		"sessions":      len(h.sessions),
		"devices":       h.reg.Count(),
		"pairWindows":   h.pairs.Open(),
	}
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return idkey.EncodeB64(buf), nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
