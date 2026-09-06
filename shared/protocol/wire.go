// Package protocol defines the ALL SHARE wire formats.
//
// There are three distinct layers:
//
//	signal.go  — JSON control plane spoken to the rendezvous server (SDP, presence, wake).
//	wire.go    — binary data plane spoken peer-to-peer over WebRTC data channels.
//	keys.go    — the keyboard code space (USB HID usage IDs).
//
// The binary data plane is deliberately tiny and allocation-light: an input
// packet is 15–48 bytes, which keeps a 1000 Hz mouse well under 50 kbit/s and
// lets every packet fit in a single SCTP chunk.
//
// Endianness is little-endian throughout. That matches x86 hosts and lets the
// browser use DataView with littleEndian=true, so neither side pays for byte
// swapping on the hot path.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Version is the data-plane protocol version. Peers refuse to talk across a
// major mismatch; minor bumps must stay backward compatible.
const (
	VersionMajor = 1
	VersionMinor = 0
)

// ErrShort is returned when a buffer is too small to hold the declared message.
var ErrShort = errors.New("allshare/protocol: buffer too short")

// ErrUnknownType is returned for a message type this build does not implement.
// Receivers must ignore unknown types rather than dropping the connection, so
// that a newer peer can add messages without breaking an older one.
var ErrUnknownType = errors.New("allshare/protocol: unknown message type")

// Message type identifiers.
//
// 0x01–0x3F  client → agent on the "input" channel (unreliable, unordered)
// 0x80–0xBF  agent  → client on the "ctrl" channel (reliable, ordered)
// 0xC0–0xFF  client → agent on the "ctrl" channel (reliable, ordered)
const (
	// Input plane (client → agent).
	TypeMouseMoveAbs uint8 = 0x01
	TypeMouseMoveRel uint8 = 0x02
	TypeMouseButton  uint8 = 0x03
	TypeMouseWheel   uint8 = 0x04
	TypeKey          uint8 = 0x05
	TypeKeyStateSync uint8 = 0x06
	TypeInputPing    uint8 = 0x07
	TypeTextInput    uint8 = 0x08

	// Control plane (agent → client).
	TypeHello          uint8 = 0x80
	TypeStats          uint8 = 0x81
	TypeCursorShape    uint8 = 0x82
	TypeCursorState    uint8 = 0x83
	TypeClipboardOut   uint8 = 0x84
	TypeDisplayChanged uint8 = 0x85
	TypeFrameMark      uint8 = 0x86
	TypePong           uint8 = 0x87
	TypeNotice         uint8 = 0x88
	TypeControlState   uint8 = 0x89

	// Control plane (client → agent).
	TypeSetQuality      uint8 = 0xC0
	TypeSelectMonitor   uint8 = 0xC1
	TypeClipboardIn     uint8 = 0xC2
	TypeRequestKeyframe uint8 = 0xC3
	TypeSetCursorMode   uint8 = 0xC4
	TypeCtrlPing        uint8 = 0xC5
	TypeViewport        uint8 = 0xC6
	TypeDisconnect      uint8 = 0xC7
	TypeRequestControl  uint8 = 0xC8
	TypeSetPointerMode  uint8 = 0xC9
)

// KeyBitmapBytes is the size of the held-key bitmap: one bit per USB HID usage
// ID in the keyboard page (0x00–0xFF).
//
// Every input packet that can change key state carries the complete bitmap.
// That makes the unreliable input channel self-healing: a dropped key event is
// corrected by the very next packet instead of leaving a key stuck down, which
// is the single most common failure mode in remote-desktop input paths.
const KeyBitmapBytes = 32

// Mouse button bit positions used by the buttons mask.
const (
	ButtonLeft uint8 = 1 << iota
	ButtonRight
	ButtonMiddle
	ButtonX1
	ButtonX2
)

// WheelTicksPerNotch matches the Windows WHEEL_DELTA constant. Sending
// high-resolution sub-notch deltas preserves smooth trackpad scrolling.
const WheelTicksPerNotch = 120

// KeyBitmap is the set of currently-held keys, indexed by HID usage ID.
type KeyBitmap [KeyBitmapBytes]byte

// Set marks a HID usage as held or released.
func (k *KeyBitmap) Set(usage uint16, down bool) {
	if usage >= KeyBitmapBytes*8 {
		return
	}
	mask := byte(1) << (usage % 8)
	if down {
		k[usage/8] |= mask
	} else {
		k[usage/8] &^= mask
	}
}

// Get reports whether a HID usage is held.
func (k *KeyBitmap) Get(usage uint16) bool {
	if usage >= KeyBitmapBytes*8 {
		return false
	}
	return k[usage/8]&(byte(1)<<(usage%8)) != 0
}

// Any reports whether any key is held.
func (k *KeyBitmap) Any() bool {
	for _, b := range k {
		if b != 0 {
			return true
		}
	}
	return false
}

// Diff calls fn for every usage whose state differs between k and next.
// The agent uses this to turn an authoritative state snapshot into the minimal
// set of SendInput calls needed to reach it.
func (k *KeyBitmap) Diff(next *KeyBitmap, fn func(usage uint16, down bool)) {
	for i := 0; i < KeyBitmapBytes; i++ {
		delta := k[i] ^ next[i]
		if delta == 0 {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if delta&(1<<bit) == 0 {
				continue
			}
			usage := uint16(i*8 + bit)
			fn(usage, next[i]&(1<<bit) != 0)
		}
	}
}

// ---------------------------------------------------------------------------
// Input plane
// ---------------------------------------------------------------------------

// PointerMode selects how the client reports mouse position.
type PointerMode uint8

const (
	// PointerAbsolute reports normalized screen coordinates. Correct for
	// desktop use: the remote cursor lands exactly where the user points, and
	// a lost packet self-corrects on the next move.
	PointerAbsolute PointerMode = 0
	// PointerRelative reports raw deltas from Pointer Lock. Required for
	// first-person games, 3D viewports and anything reading raw mouse input.
	PointerRelative PointerMode = 1
)

// MouseMoveAbs is an absolute pointer position.
//
// X and Y are normalized to 0..65535 across the captured surface rather than
// sent as pixels. That makes the packet independent of both the stream
// resolution and any mid-session resolution change, so a resolution switch can
// never misplace the cursor.
type MouseMoveAbs struct {
	Seq     uint32
	TSMilli uint32
	X       uint16
	Y       uint16
	Buttons uint8
}

// SizeMouseMoveAbs is the encoded size of MouseMoveAbs including the type byte.
const SizeMouseMoveAbs = 1 + 4 + 4 + 2 + 2 + 1

// Encode appends the encoded message to dst.
func (m MouseMoveAbs) Encode(dst []byte) []byte {
	dst = append(dst, TypeMouseMoveAbs)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = appendU16(dst, m.X)
	dst = appendU16(dst, m.Y)
	return append(dst, m.Buttons)
}

// DecodeMouseMoveAbs parses the payload following the type byte.
func DecodeMouseMoveAbs(b []byte) (MouseMoveAbs, error) {
	var m MouseMoveAbs
	if len(b) < SizeMouseMoveAbs-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.X = binary.LittleEndian.Uint16(b[8:])
	m.Y = binary.LittleEndian.Uint16(b[10:])
	m.Buttons = b[12]
	return m, nil
}

// MouseMoveRel is a raw relative pointer delta captured under Pointer Lock.
type MouseMoveRel struct {
	Seq     uint32
	TSMilli uint32
	DX      int16
	DY      int16
	Buttons uint8
}

// SizeMouseMoveRel is the encoded size of MouseMoveRel including the type byte.
const SizeMouseMoveRel = 1 + 4 + 4 + 2 + 2 + 1

// Encode appends the encoded message to dst.
func (m MouseMoveRel) Encode(dst []byte) []byte {
	dst = append(dst, TypeMouseMoveRel)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = appendU16(dst, uint16(m.DX))
	dst = appendU16(dst, uint16(m.DY))
	return append(dst, m.Buttons)
}

// DecodeMouseMoveRel parses the payload following the type byte.
func DecodeMouseMoveRel(b []byte) (MouseMoveRel, error) {
	var m MouseMoveRel
	if len(b) < SizeMouseMoveRel-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.DX = int16(binary.LittleEndian.Uint16(b[8:]))
	m.DY = int16(binary.LittleEndian.Uint16(b[10:]))
	m.Buttons = b[12]
	return m, nil
}

// MouseButton is a button transition. Buttons carries the full post-transition
// mask so a dropped packet cannot leave a button stuck down.
type MouseButton struct {
	Seq     uint32
	TSMilli uint32
	Button  uint8
	Down    bool
	Buttons uint8
}

// SizeMouseButton is the encoded size of MouseButton including the type byte.
const SizeMouseButton = 1 + 4 + 4 + 1 + 1 + 1

// Encode appends the encoded message to dst.
func (m MouseButton) Encode(dst []byte) []byte {
	dst = append(dst, TypeMouseButton)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = append(dst, m.Button, boolByte(m.Down), m.Buttons)
	return dst
}

// DecodeMouseButton parses the payload following the type byte.
func DecodeMouseButton(b []byte) (MouseButton, error) {
	var m MouseButton
	if len(b) < SizeMouseButton-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.Button = b[8]
	m.Down = b[9] != 0
	m.Buttons = b[10]
	return m, nil
}

// MouseWheel carries high-resolution scroll deltas in 1/120 notch units.
type MouseWheel struct {
	Seq     uint32
	TSMilli uint32
	DX      int32
	DY      int32
}

// SizeMouseWheel is the encoded size of MouseWheel including the type byte.
const SizeMouseWheel = 1 + 4 + 4 + 4 + 4

// Encode appends the encoded message to dst.
func (m MouseWheel) Encode(dst []byte) []byte {
	dst = append(dst, TypeMouseWheel)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = appendU32(dst, uint32(m.DX))
	dst = appendU32(dst, uint32(m.DY))
	return dst
}

// DecodeMouseWheel parses the payload following the type byte.
func DecodeMouseWheel(b []byte) (MouseWheel, error) {
	var m MouseWheel
	if len(b) < SizeMouseWheel-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.DX = int32(binary.LittleEndian.Uint32(b[8:]))
	m.DY = int32(binary.LittleEndian.Uint32(b[12:]))
	return m, nil
}

// Key is a key transition plus the complete post-transition key bitmap.
//
// Carrying the full state costs 32 bytes per key event but removes an entire
// class of bug: no sequence of drops, reorders or focus changes can leave the
// remote machine with a key held down.
type Key struct {
	Seq     uint32
	TSMilli uint32
	Usage   uint16
	Down    bool
	State   KeyBitmap
}

// SizeKey is the encoded size of Key including the type byte.
const SizeKey = 1 + 4 + 4 + 2 + 1 + KeyBitmapBytes

// Encode appends the encoded message to dst.
func (m Key) Encode(dst []byte) []byte {
	dst = append(dst, TypeKey)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = appendU16(dst, m.Usage)
	dst = append(dst, boolByte(m.Down))
	return append(dst, m.State[:]...)
}

// DecodeKey parses the payload following the type byte.
func DecodeKey(b []byte) (Key, error) {
	var m Key
	if len(b) < SizeKey-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.Usage = binary.LittleEndian.Uint16(b[8:])
	m.Down = b[10] != 0
	copy(m.State[:], b[11:11+KeyBitmapBytes])
	return m, nil
}

// KeyStateSync is an authoritative snapshot of all input state.
//
// The client sends it on focus loss, on Pointer Lock exit, on reconnect and as
// a low-rate heartbeat. An all-zero snapshot is the "release everything" signal.
type KeyStateSync struct {
	Seq     uint32
	TSMilli uint32
	Buttons uint8
	State   KeyBitmap
}

// SizeKeyStateSync is the encoded size of KeyStateSync including the type byte.
const SizeKeyStateSync = 1 + 4 + 4 + 1 + KeyBitmapBytes

// Encode appends the encoded message to dst.
func (m KeyStateSync) Encode(dst []byte) []byte {
	dst = append(dst, TypeKeyStateSync)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, m.TSMilli)
	dst = append(dst, m.Buttons)
	return append(dst, m.State[:]...)
}

// DecodeKeyStateSync parses the payload following the type byte.
func DecodeKeyStateSync(b []byte) (KeyStateSync, error) {
	var m KeyStateSync
	if len(b) < SizeKeyStateSync-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMilli = binary.LittleEndian.Uint32(b[4:])
	m.Buttons = b[8]
	copy(m.State[:], b[9:9+KeyBitmapBytes])
	return m, nil
}

// TextInput carries literal text to type, used by the "match my keyboard
// layout" mode and by clipboard-free paste. The agent injects it as Unicode
// rather than as scancodes.
type TextInput struct {
	Seq  uint32
	Text string
}

// Encode appends the encoded message to dst.
func (m TextInput) Encode(dst []byte) []byte {
	dst = append(dst, TypeTextInput)
	dst = appendU32(dst, m.Seq)
	dst = appendU32(dst, uint32(len(m.Text)))
	return append(dst, m.Text...)
}

// DecodeTextInput parses the payload following the type byte.
func DecodeTextInput(b []byte, maxLen int) (TextInput, error) {
	var m TextInput
	if len(b) < 8 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if n < 0 || n > maxLen {
		return m, fmt.Errorf("allshare/protocol: text length %d exceeds limit %d", n, maxLen)
	}
	if len(b) < 8+n {
		return m, ErrShort
	}
	m.Text = string(b[8 : 8+n])
	return m, nil
}

// InputPing measures the input path independently of the video path.
type InputPing struct {
	Seq   uint32
	TSMic uint64
}

// SizeInputPing is the encoded size of InputPing including the type byte.
const SizeInputPing = 1 + 4 + 8

// Encode appends the encoded message to dst.
func (m InputPing) Encode(dst []byte) []byte {
	dst = append(dst, TypeInputPing)
	dst = appendU32(dst, m.Seq)
	return appendU64(dst, m.TSMic)
}

// DecodeInputPing parses the payload following the type byte.
func DecodeInputPing(b []byte) (InputPing, error) {
	var m InputPing
	if len(b) < SizeInputPing-1 {
		return m, ErrShort
	}
	m.Seq = binary.LittleEndian.Uint32(b[0:])
	m.TSMic = binary.LittleEndian.Uint64(b[4:])
	return m, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func appendU16(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}

func appendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendU64(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

func appendF32(dst []byte, v float32) []byte {
	return appendU32(dst, math.Float32bits(v))
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// NormalizeCoord converts a pixel coordinate into the 0..65535 normalized space
// used by MouseMoveAbs. size is the extent of the captured surface in pixels.
func NormalizeCoord(pixel, size int) uint16 {
	if size <= 1 {
		return 0
	}
	if pixel < 0 {
		pixel = 0
	}
	if pixel > size-1 {
		pixel = size - 1
	}
	return uint16((int64(pixel) * 65535) / int64(size-1))
}

// DenormalizeCoord is the inverse of NormalizeCoord.
func DenormalizeCoord(norm uint16, size int) int {
	if size <= 1 {
		return 0
	}
	return int((int64(norm)*int64(size-1) + 32767) / 65535)
}
