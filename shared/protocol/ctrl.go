package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// The control channel mixes two encodings on purpose.
//
// Structural, low-rate messages (Hello, Stats, quality changes) use a type byte
// followed by JSON: they are self-describing, trivially extensible and easy to
// inspect in a debug log, and at 1–2 Hz their cost is irrelevant.
//
// Per-frame messages (CursorState, FrameMark) are fixed-layout binary: they can
// run at the video frame rate, and JSON parsing 60 times a second on a
// Chromebook's main thread is a real cost worth avoiding.

// MaxCtrlMessage bounds any single control-channel message. Chrome reports an
// SCTP maxMessageSize of 262144; staying under it avoids fragmentation and
// caps the memory a hostile peer can make us allocate.
const MaxCtrlMessage = 192 * 1024

// MaxClipboardBytes bounds a clipboard transfer in either direction.
const MaxClipboardBytes = 64 * 1024

// MaxTextInputBytes bounds a single typed-text injection.
const MaxTextInputBytes = 8 * 1024

// EncodeJSON frames a JSON control message: one type byte then the document.
func EncodeJSON(msgType uint8, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("allshare/protocol: encode type 0x%02x: %w", msgType, err)
	}
	out := make([]byte, 0, len(body)+1)
	out = append(out, msgType)
	return append(out, body...), nil
}

// DecodeJSON parses the payload following the type byte into v.
func DecodeJSON(payload []byte, v any) error {
	if len(payload) > MaxCtrlMessage {
		return fmt.Errorf("allshare/protocol: control message of %d bytes exceeds limit", len(payload))
	}
	return json.Unmarshal(payload, v)
}

// ---------------------------------------------------------------------------
// Agent → client
// ---------------------------------------------------------------------------

// Monitor describes one display attached to the remote machine.
type Monitor struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Primary bool   `json:"primary"`
	// RefreshHz is the display's refresh rate, used to cap the capture rate at
	// something the compositor can actually produce.
	RefreshHz int `json:"refreshHz"`
	// ScalePercent is the Windows display scaling (100 = 100%). The client uses
	// it to decide whether downscaling would make text unreadable.
	ScalePercent int `json:"scalePercent"`
}

// Hello is the first control message the agent sends. It tells the client
// everything it needs to render, size and label the session.
type Hello struct {
	VersionMajor int    `json:"versionMajor"`
	VersionMinor int    `json:"versionMinor"`
	AgentVersion string `json:"agentVersion"`
	DeviceName   string `json:"deviceName"`

	Monitors        []Monitor `json:"monitors"`
	ActiveMonitor   int       `json:"activeMonitor"`
	StreamWidth     int       `json:"streamWidth"`
	StreamHeight    int       `json:"streamHeight"`
	Codec           string    `json:"codec"`
	CodecProfile    string    `json:"codecProfile"`
	Encoder         string    `json:"encoder"`
	CaptureBackend  string    `json:"captureBackend"`
	HardwareEncoded bool      `json:"hardwareEncoded"`

	HasAudio       bool `json:"hasAudio"`
	HasClipboard   bool `json:"hasClipboard"`
	CanSetMonitor  bool `json:"canSetMonitor"`
	CursorEmbedded bool `json:"cursorEmbedded"`

	// HasControl reports whether this client may send input, or is a viewer.
	HasControl bool `json:"hasControl"`
	// SessionKind is "desktop", "lockscreen" or "login" — the client shows an
	// honest badge rather than pretending a locked machine is a usable desktop.
	SessionKind string `json:"sessionKind"`
}

// Stats is the agent's periodic view of its own pipeline. The client merges it
// with browser-side WebRTC stats to build the performance HUD, so the user sees
// one latency budget rather than two halves of one.
type Stats struct {
	TSMilli uint32 `json:"t"`

	CaptureFPS  float32 `json:"capFps"`
	EncodeFPS   float32 `json:"encFps"`
	SentKbps    float32 `json:"kbps"`
	TargetKbps  float32 `json:"targetKbps"`
	EncoderQP   float32 `json:"qp"`
	Width       int     `json:"w"`
	Height      int     `json:"h"`
	IdleSkipped uint32  `json:"idleSkipped"`

	CaptureMs float32 `json:"capMs"`
	EncodeMs  float32 `json:"encMs"`
	QueueMs   float32 `json:"qMs"`

	// LastInputSeq and LastInputRecvMilli let the client measure the input path
	// on its own clock without needing the two machines to share a time base.
	LastInputSeq       uint32 `json:"inSeq"`
	LastInputTSMilli   uint32 `json:"inTs"`
	InputToInjectMicro uint32 `json:"inInjectUs"`

	CPUPercent float32 `json:"cpu"`
	GPUPercent float32 `json:"gpu"`
	RSSMB      float32 `json:"rssMb"`
}

// CursorShapeMeta accompanies a cursor bitmap. The bitmap itself follows the
// JSON document so the image bytes never pay base64's 33% overhead.
type CursorShapeMeta struct {
	ShapeID uint32 `json:"id"`
	Width   int    `json:"w"`
	Height  int    `json:"h"`
	HotX    int    `json:"hx"`
	HotY    int    `json:"hy"`
	Format  string `json:"fmt"` // "png"
	Bytes   int    `json:"n"`
}

// EncodeCursorShape frames cursor metadata plus its PNG payload.
func EncodeCursorShape(meta CursorShapeMeta, png []byte) ([]byte, error) {
	meta.Bytes = len(png)
	head, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if len(head)+len(png)+5 > MaxCtrlMessage {
		return nil, fmt.Errorf("allshare/protocol: cursor shape too large (%d bytes)", len(png))
	}
	out := make([]byte, 0, 5+len(head)+len(png))
	out = append(out, TypeCursorShape)
	out = appendU32(out, uint32(len(head)))
	out = append(out, head...)
	return append(out, png...), nil
}

// DecodeCursorShape parses the payload following the type byte.
func DecodeCursorShape(b []byte) (CursorShapeMeta, []byte, error) {
	var meta CursorShapeMeta
	if len(b) < 4 {
		return meta, nil, ErrShort
	}
	n := int(binary.LittleEndian.Uint32(b[0:]))
	if n < 0 || 4+n > len(b) {
		return meta, nil, ErrShort
	}
	if err := json.Unmarshal(b[4:4+n], &meta); err != nil {
		return meta, nil, err
	}
	return meta, b[4+n:], nil
}

// CursorState is the agent's authoritative cursor position and shape.
//
// It runs at frame rate because the client renders the cursor locally: drawing
// it from the client's own pointer position makes cursor motion feel instant
// regardless of round-trip time, and this message is what keeps that local
// cursor honest (shape changes, programmatic warps, confinement in games).
type CursorState struct {
	Seq     uint32
	ShapeID uint32
	X       uint16 // normalized 0..65535
	Y       uint16
	Visible bool
	// Relative is set while the remote application has captured the pointer, so
	// the client knows to stop trusting absolute positioning.
	Relative bool
}

// SizeCursorState is the encoded size of CursorState including the type byte.
const SizeCursorState = 1 + 4 + 4 + 2 + 2 + 1 + 1

// Encode appends the encoded message to dst.
func (c CursorState) Encode(dst []byte) []byte {
	dst = append(dst, TypeCursorState)
	dst = appendU32(dst, c.Seq)
	dst = appendU32(dst, c.ShapeID)
	dst = appendU16(dst, c.X)
	dst = appendU16(dst, c.Y)
	return append(dst, boolByte(c.Visible), boolByte(c.Relative))
}

// DecodeCursorState parses the payload following the type byte.
func DecodeCursorState(b []byte) (CursorState, error) {
	var c CursorState
	if len(b) < SizeCursorState-1 {
		return c, ErrShort
	}
	c.Seq = binary.LittleEndian.Uint32(b[0:])
	c.ShapeID = binary.LittleEndian.Uint32(b[4:])
	c.X = binary.LittleEndian.Uint16(b[8:])
	c.Y = binary.LittleEndian.Uint16(b[10:])
	c.Visible = b[12] != 0
	c.Relative = b[13] != 0
	return c, nil
}

// FrameMark ties one encoded video frame to the input that caused it.
//
// The client matches RTPTimestamp against the metadata delivered by
// requestVideoFrameCallback, which is what makes a true end-to-end
// "key press to pixels on screen" measurement possible in a browser.
type FrameMark struct {
	RTPTimestamp uint32
	InputSeq     uint32
	CaptureMicro uint64
	EncodeMicro  uint32
	SizeBytes    uint32
	Keyframe     bool
}

// SizeFrameMark is the encoded size of FrameMark including the type byte.
const SizeFrameMark = 1 + 4 + 4 + 8 + 4 + 4 + 1

// Encode appends the encoded message to dst.
func (f FrameMark) Encode(dst []byte) []byte {
	dst = append(dst, TypeFrameMark)
	dst = appendU32(dst, f.RTPTimestamp)
	dst = appendU32(dst, f.InputSeq)
	dst = appendU64(dst, f.CaptureMicro)
	dst = appendU32(dst, f.EncodeMicro)
	dst = appendU32(dst, f.SizeBytes)
	return append(dst, boolByte(f.Keyframe))
}

// DecodeFrameMark parses the payload following the type byte.
func DecodeFrameMark(b []byte) (FrameMark, error) {
	var f FrameMark
	if len(b) < SizeFrameMark-1 {
		return f, ErrShort
	}
	f.RTPTimestamp = binary.LittleEndian.Uint32(b[0:])
	f.InputSeq = binary.LittleEndian.Uint32(b[4:])
	f.CaptureMicro = binary.LittleEndian.Uint64(b[8:])
	f.EncodeMicro = binary.LittleEndian.Uint32(b[16:])
	f.SizeBytes = binary.LittleEndian.Uint32(b[20:])
	f.Keyframe = b[24] != 0
	return f, nil
}

// Pong answers InputPing or CtrlPing.
type Pong struct {
	Seq         uint32
	ClientTSMic uint64
	AgentTSMic  uint64
}

// SizePong is the encoded size of Pong including the type byte.
const SizePong = 1 + 4 + 8 + 8

// Encode appends the encoded message to dst.
func (p Pong) Encode(dst []byte) []byte {
	dst = append(dst, TypePong)
	dst = appendU32(dst, p.Seq)
	dst = appendU64(dst, p.ClientTSMic)
	return appendU64(dst, p.AgentTSMic)
}

// DecodePong parses the payload following the type byte.
func DecodePong(b []byte) (Pong, error) {
	var p Pong
	if len(b) < SizePong-1 {
		return p, ErrShort
	}
	p.Seq = binary.LittleEndian.Uint32(b[0:])
	p.ClientTSMic = binary.LittleEndian.Uint64(b[4:])
	p.AgentTSMic = binary.LittleEndian.Uint64(b[12:])
	return p, nil
}

// NoticeSeverity classifies a Notice for presentation.
type NoticeSeverity string

// Notice severities.
const (
	NoticeInfo    NoticeSeverity = "info"
	NoticeWarning NoticeSeverity = "warning"
	NoticeError   NoticeSeverity = "error"
)

// Notice is a human-readable message from the agent.
//
// Code is for logs and support; Message is what the user reads, and must be
// written in plain language with no protocol jargon.
type Notice struct {
	Severity NoticeSeverity `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Detail   string         `json:"detail,omitempty"`
}

// ControlState reports whether this client currently holds input control.
type ControlState struct {
	HasControl  bool   `json:"hasControl"`
	HolderLabel string `json:"holderLabel,omitempty"`
	ViewerCount int    `json:"viewerCount"`
}

// DisplayChanged is sent when monitors are added, removed or resized.
type DisplayChanged struct {
	Monitors      []Monitor `json:"monitors"`
	ActiveMonitor int       `json:"activeMonitor"`
	StreamWidth   int       `json:"streamWidth"`
	StreamHeight  int       `json:"streamHeight"`
	SessionKind   string    `json:"sessionKind"`
}

// Clipboard carries UTF-8 text in either direction.
type Clipboard struct {
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// Client → agent
// ---------------------------------------------------------------------------

// QualityPreset names a bundle of streaming trade-offs.
type QualityPreset string

// Quality presets.
const (
	// PresetGaming favours motion and input response: high frame rate, small
	// jitter buffer, frequent small frames.
	PresetGaming QualityPreset = "gaming"
	// PresetDesktop favours still-image clarity: native resolution, low frame
	// rate when idle, more bits per changed pixel so text stays crisp.
	PresetDesktop QualityPreset = "desktop"
	// PresetBalanced is the default middle ground.
	PresetBalanced QualityPreset = "balanced"
	// PresetCustom means the explicit fields below govern.
	PresetCustom QualityPreset = "custom"
)

// ResolutionPolicy selects how the stream resolution is chosen.
type ResolutionPolicy string

// Resolution policies.
const (
	// ResolutionNative streams the desktop pixel-for-pixel. Sharpest possible
	// text because no resampling happens anywhere in the pipeline.
	ResolutionNative ResolutionPolicy = "native"
	// ResolutionFit matches the client viewport, avoiding a scale on both ends.
	ResolutionFit ResolutionPolicy = "fit"
	// ResolutionFixed uses the explicit Width/Height.
	ResolutionFixed ResolutionPolicy = "fixed"
	// ResolutionAuto lets the agent choose from bandwidth and viewport.
	ResolutionAuto ResolutionPolicy = "auto"
)

// SetQuality asks the agent to change streaming parameters. Every field is
// advisory: the agent clamps to what the hardware and network can sustain.
type SetQuality struct {
	Preset     QualityPreset    `json:"preset"`
	Resolution ResolutionPolicy `json:"resolution"`
	Width      int              `json:"width,omitempty"`
	Height     int              `json:"height,omitempty"`
	MaxFPS     int              `json:"maxFps,omitempty"`
	MaxKbps    int              `json:"maxKbps,omitempty"`
	MinKbps    int              `json:"minKbps,omitempty"`
	Adaptive   bool             `json:"adaptive"`
	// AudioEnabled lets the agent stop capturing audio entirely when muted,
	// rather than encoding sound nobody is listening to.
	AudioEnabled bool `json:"audioEnabled"`
}

// Viewport reports the client's drawable size so ResolutionFit and
// ResolutionAuto have something real to aim at.
type Viewport struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	DPR    float64 `json:"dpr"`
	// DecodeBudgetMs is the client's measured decode time per frame. When it
	// climbs, the agent lowers resolution before the client starts dropping
	// frames, which is much less visible than a stutter.
	DecodeBudgetMs float64 `json:"decodeBudgetMs,omitempty"`
}

// SelectMonitor switches which display is captured.
type SelectMonitor struct {
	MonitorID int `json:"monitorId"`
}

// SetCursorMode controls where the cursor is drawn.
type SetCursorMode struct {
	// LocalCursor asks the agent to exclude the cursor from the video and send
	// shape updates instead, so the client can draw it with zero latency.
	LocalCursor bool `json:"localCursor"`
}

// SetPointerMode switches absolute and relative pointer reporting.
type SetPointerMode struct {
	Mode string `json:"mode"` // "absolute" | "relative"
}

// RequestControl asks to take input control from a viewer slot.
type RequestControl struct {
	Take bool `json:"take"`
}
