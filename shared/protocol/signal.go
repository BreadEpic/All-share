package protocol

import "encoding/json"

// The rendezvous protocol is JSON over a WebSocket. It is deliberately thin:
// the server routes messages and tracks presence, and is *not* trusted with
// confidentiality or integrity of anything that matters.
//
// Every session-establishing message is signed end-to-end with the peers'
// Ed25519 identity keys, and the signature covers the SDP — which contains the
// DTLS fingerprint. A malicious or compromised rendezvous can therefore deny
// service, but it cannot insert itself into a session, read the screen, or
// inject input. That property is what makes it reasonable to use a shared or
// third-party rendezvous instead of demanding everyone self-host.

// SignalVersion is the rendezvous protocol version, sent in Hello.
const SignalVersion = 1

// Role identifies which side of the rendezvous a connection is.
type Role string

// Rendezvous roles.
const (
	RoleAgent  Role = "agent"
	RoleClient Role = "client"
)

// Envelope is the outer frame of every rendezvous message.
type Envelope struct {
	Type string          `json:"t"`
	Ref  string          `json:"ref,omitempty"`
	Body json.RawMessage `json:"b,omitempty"`
}

// Rendezvous message types.
const (
	// Handshake.
	MsgHello     = "hello"
	MsgChallenge = "challenge"
	MsgAuth      = "auth"
	MsgAuthOK    = "authOk"
	MsgError     = "error"
	MsgPingRV    = "ping"
	MsgPongRV    = "pong"

	// Agent lifecycle.
	MsgAgentRegister = "agentRegister"
	MsgAgentUpdate   = "agentUpdate"
	MsgWakePeer      = "wakePeer"

	// Pairing.
	MsgPairPublish = "pairPublish"
	MsgPairRevoke  = "pairRevoke"
	MsgPairLookup  = "pairLookup"
	MsgPairOffer   = "pairOffer"
	MsgPairSubmit  = "pairSubmit"
	MsgPairResult  = "pairResult"
	MsgPairAccept  = "pairAccept"

	// Discovery and presence.
	MsgListDevices  = "listDevices"
	MsgDevices      = "devices"
	MsgDeviceUpdate = "deviceUpdate"
	MsgForgetDevice = "forgetDevice"

	// Session establishment.
	MsgConnect        = "connect"
	MsgConnectRequest = "connectRequest"
	MsgSignal         = "signal"
	MsgSessionEnd     = "sessionEnd"

	// Wake.
	MsgWake       = "wake"
	MsgWakeStatus = "wakeStatus"
	MsgStayAwake  = "stayAwake"
)

// ErrorCode is a stable machine-readable failure reason.
//
// The client maps every code to a plain-language sentence; the code itself is
// only ever shown under "Advanced details".
type ErrorCode string

// Rendezvous error codes.
const (
	ErrBadRequest      ErrorCode = "bad_request"
	ErrUnauthorized    ErrorCode = "unauthorized"
	ErrRateLimited     ErrorCode = "rate_limited"
	ErrDeviceOffline   ErrorCode = "device_offline"
	ErrDeviceUnknown   ErrorCode = "device_unknown"
	ErrNotPaired       ErrorCode = "not_paired"
	ErrPairExpired     ErrorCode = "pair_expired"
	ErrPairBadCode     ErrorCode = "pair_bad_code"
	ErrPairInUse       ErrorCode = "pair_in_use"
	ErrBusy            ErrorCode = "busy"
	ErrWakeUnavailable ErrorCode = "wake_unavailable"
	ErrInternal        ErrorCode = "internal"
	ErrVersion         ErrorCode = "version_mismatch"
)

// RVHello opens a rendezvous connection.
type RVHello struct {
	Version  int    `json:"version"`
	Role     Role   `json:"role"`
	DeviceID string `json:"deviceId"` // base64url Ed25519 public key
	Agent    string `json:"agent"`    // implementation string, for logs
}

// Challenge carries the server nonce a client must sign.
type Challenge struct {
	Nonce     string `json:"nonce"` // base64url, 32 bytes
	ServerID  string `json:"serverId"`
	ServerNow int64  `json:"now"`
}

// Auth answers a Challenge.
type Auth struct {
	DeviceID  string `json:"deviceId"`
	Nonce     string `json:"nonce"`
	Signature string `json:"sig"`
}

// ICEServer mirrors the browser RTCIceServer shape.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// AuthOK completes the handshake and hands out relay credentials.
type AuthOK struct {
	SessionID  string      `json:"sessionId"`
	ICEServers []ICEServer `json:"iceServers"`
	// TURNExpiresAt lets a long-lived client refresh credentials before a
	// reconnect fails, instead of discovering the expiry mid-outage.
	TURNExpiresAt int64  `json:"turnExpiresAt,omitempty"`
	ServerNow     int64  `json:"now"`
	ServerVersion string `json:"serverVersion"`
}

// ErrorBody is the payload of MsgError.
type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Retry   int       `json:"retryAfter,omitempty"`
}

// WakeCapability describes how, if at all, a sleeping machine can be woken.
type WakeCapability struct {
	// Method is "none", "checkin", "lanPeer", "wolDirect" or "modernStandby".
	Method string `json:"method"`
	// EstimatedSeconds is an honest upper bound on how long a wake may take.
	// For the check-in method this is the check-in interval, and the UI shows
	// it rather than implying an instant wake it cannot deliver.
	EstimatedSeconds int `json:"estimatedSeconds"`
	// MACAddresses are the wake-armed NICs, used by a LAN peer to target the
	// magic packet. They are visible only to devices paired with this agent.
	MACAddresses []string `json:"macs,omitempty"`
	// LANKey groups agents that share a local network, so the server can pick a
	// peer capable of delivering a magic packet. It is a salted hash of the
	// public egress address, never the address itself.
	LANKey string `json:"lanKey,omitempty"`
	// BroadcastAddrs are the subnet broadcast addresses a LAN peer should use.
	BroadcastAddrs []string `json:"broadcast,omitempty"`
	// Armed reports whether Windows currently lists a NIC as wake-armed.
	Armed bool `json:"armed"`
	// Reason explains an unavailable wake path in plain language.
	Reason string `json:"reason,omitempty"`
}

// AgentRegister announces an agent's identity and capabilities.
type AgentRegister struct {
	Name         string         `json:"name"`
	OS           string         `json:"os"`
	AgentVersion string         `json:"agentVersion"`
	Wake         WakeCapability `json:"wake"`
	// PairedClients lets the server answer "which of my PCs is this?" for a
	// client without the agent having to be online. Only key fingerprints are
	// shared, never names or any other client detail.
	PairedClients []string `json:"pairedClients,omitempty"`
	Busy          bool     `json:"busy"`
}

// AgentUpdate is a lighter-weight status refresh.
type AgentUpdate struct {
	Busy        bool            `json:"busy"`
	SessionKind string          `json:"sessionKind,omitempty"`
	Wake        *WakeCapability `json:"wake,omitempty"`
}

// DeviceInfo is what a client is told about one of its paired machines.
type DeviceInfo struct {
	DeviceID     string         `json:"deviceId"`
	Name         string         `json:"name"`
	Online       bool           `json:"online"`
	Busy         bool           `json:"busy"`
	LastSeen     int64          `json:"lastSeen"`
	AgentVersion string         `json:"agentVersion,omitempty"`
	OS           string         `json:"os,omitempty"`
	Wake         WakeCapability `json:"wake"`
}

// DevicesBody answers MsgListDevices.
type DevicesBody struct {
	Devices []DeviceInfo `json:"devices"`
}

// PairPublish opens a pairing window. The agent sends it; the server stores it
// under CodeID and learns nothing about the pairing code itself.
type PairPublish struct {
	CodeID       string `json:"codeId"` // base64url, PBKDF2-derived handle
	Salt         string `json:"salt"`   // base64url, 16 random bytes
	EphemeralPub string `json:"epk"`    // base64url X25519 public key
	IdentityPub  string `json:"idPub"`  // base64url Ed25519 identity key
	DeviceName   string `json:"name"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// PairLookup is the client's request for an open pairing window.
type PairLookup struct {
	CodeID string `json:"codeId"`
}

// PairOffer answers PairLookup with the agent's half of the exchange.
type PairOffer struct {
	CodeID       string `json:"codeId"`
	Salt         string `json:"salt"`
	EphemeralPub string `json:"epk"`
	IdentityPub  string `json:"idPub"`
	DeviceName   string `json:"name"`
	DeviceID     string `json:"deviceId"`
}

// PairSubmit is the client's half plus its key-confirmation tag.
type PairSubmit struct {
	CodeID       string `json:"codeId"`
	EphemeralPub string `json:"epk"`
	IdentityPub  string `json:"idPub"`
	ClientLabel  string `json:"label"`
	Confirm      string `json:"confirm"`
}

// PairResult reports the outcome to the client.
type PairResult struct {
	OK          bool      `json:"ok"`
	Code        ErrorCode `json:"code,omitempty"`
	Confirm     string    `json:"confirm,omitempty"`
	DeviceID    string    `json:"deviceId,omitempty"`
	DeviceName  string    `json:"name,omitempty"`
	IdentityPub string    `json:"idPub,omitempty"`
}

// ConnectBody asks the server to introduce a client to an agent.
type ConnectBody struct {
	Target string `json:"target"`
	// Codecs is the browser's real decode capability, taken from
	// RTCRtpReceiver.getCapabilities. The agent intersects it with what its
	// hardware can encode instead of assuming a codec is available: this is why
	// the same client works on a Chromebook with H.264 and on a Chromium build
	// without it.
	Codecs []CodecCapability `json:"codecs"`
	Label  string            `json:"label"`
	// Reconnect marks a resume attempt after a dropped session, so the agent can
	// keep the capture pipeline warm instead of tearing it down.
	Reconnect    bool   `json:"reconnect,omitempty"`
	PriorSession string `json:"priorSession,omitempty"`
}

// CodecCapability mirrors one entry of RTCRtpCodecCapability.
type CodecCapability struct {
	MimeType    string `json:"mimeType"`
	ClockRate   uint32 `json:"clockRate"`
	Channels    uint16 `json:"channels,omitempty"`
	SDPFmtpLine string `json:"sdpFmtpLine,omitempty"`
}

// ConnectRequest is what the agent receives when a client wants in.
type ConnectRequest struct {
	SessionID string            `json:"sessionId"`
	ClientID  string            `json:"clientId"`
	Label     string            `json:"label"`
	Codecs    []CodecCapability `json:"codecs"`
	Reconnect bool              `json:"reconnect,omitempty"`
}

// SignalBody relays one end-to-end signed WebRTC signalling payload.
//
// The server treats Payload and Signature as opaque. Peers verify Signature
// against the identity key pinned at pairing time before acting on Payload.
type SignalBody struct {
	SessionID string `json:"sessionId"`
	To        string `json:"to,omitempty"`
	From      string `json:"from,omitempty"`
	Payload   string `json:"payload"` // base64url of the canonical JSON below
	Signature string `json:"sig"`     // base64url Ed25519 over the signing input
}

// SignalPayload is the decoded content of SignalBody.Payload.
type SignalPayload struct {
	Kind      string  `json:"kind"` // "offer" | "answer" | "candidate" | "bye"
	SDP       string  `json:"sdp,omitempty"`
	Candidate string  `json:"candidate,omitempty"`
	SDPMid    string  `json:"sdpMid,omitempty"`
	SDPMLine  *uint16 `json:"sdpMLineIndex,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	// Nonce and Timestamp make each payload unique so a captured signalling
	// message cannot be replayed into a later session.
	Nonce     string `json:"nonce"`
	Timestamp int64  `json:"ts"`
}

// SessionEnd tears down a session from either side.
type SessionEnd struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason,omitempty"`
}

// WakeBody asks the server to wake a machine.
type WakeBody struct {
	Target string `json:"target"`
}

// WakeStatus reports progress. The client polls presence separately; this is
// the honest narration of what is actually being attempted.
type WakeStatus struct {
	Target string `json:"target"`
	// Stage is "requested", "sent", "waiting", "online", "unavailable".
	Stage            string    `json:"stage"`
	Method           string    `json:"method"`
	EstimatedSeconds int       `json:"estimatedSeconds,omitempty"`
	Code             ErrorCode `json:"code,omitempty"`
	Message          string    `json:"message,omitempty"`
}

// StayAwakeBody tells an agent that just came online that a user is on their
// way in, so it should hold off sleep long enough for the session to start.
type StayAwakeBody struct {
	Seconds int    `json:"seconds"`
	Reason  string `json:"reason,omitempty"`
}

// WakePeerBody instructs an online agent to send a magic packet for a peer that
// shares its local network.
type WakePeerBody struct {
	Target         string   `json:"target"`
	MACAddresses   []string `json:"macs"`
	BroadcastAddrs []string `json:"broadcast"`
}
