package protocol

// Keyboard input travels as USB HID usage IDs from the Keyboard/Keypad page
// (0x07), not as browser key names and not as Windows virtual-key codes.
//
// Why HID usages:
//
//   - They are a stable, physical description of which key was pressed, so the
//     mapping from KeyboardEvent.code is a pure lookup with no layout guessing.
//   - They fit in a byte, which is what makes the 256-bit held-key bitmap in
//     every input packet cheap enough to send unconditionally.
//   - They convert directly to PS/2 set-1 scancodes, and injecting scancodes
//     (rather than virtual keys) is what makes games work: DirectInput and Raw
//     Input read scancodes, and applications apply the *remote* machine's
//     keyboard layout, exactly as if the user were sitting at it.
//
// The layout-matching alternative — translating to characters on the client and
// injecting Unicode — is offered as an option for typing-heavy use across
// mismatched layouts, but it cannot drive games and is not the default.

// HID keyboard usage IDs referenced by name elsewhere in the codebase.
const (
	HIDNone  uint16 = 0x00
	HIDA     uint16 = 0x04
	HIDZ     uint16 = 0x1D
	HID1     uint16 = 0x1E
	HID0     uint16 = 0x27
	HIDEnter uint16 = 0x28
	HIDEsc   uint16 = 0x29
	HIDBack  uint16 = 0x2A
	HIDTab   uint16 = 0x2B
	HIDSpace uint16 = 0x2C

	HIDCapsLock    uint16 = 0x39
	HIDF1          uint16 = 0x3A
	HIDF12         uint16 = 0x45
	HIDPrintScreen uint16 = 0x46
	HIDScrollLock  uint16 = 0x47
	HIDPause       uint16 = 0x48
	HIDInsert      uint16 = 0x49
	HIDHome        uint16 = 0x4A
	HIDPageUp      uint16 = 0x4B
	HIDDelete      uint16 = 0x4C
	HIDEnd         uint16 = 0x4D
	HIDPageDown    uint16 = 0x4E
	HIDRight       uint16 = 0x4F
	HIDLeft        uint16 = 0x50
	HIDDown        uint16 = 0x51
	HIDUp          uint16 = 0x52
	HIDNumLock     uint16 = 0x53

	HIDControlLeft  uint16 = 0xE0
	HIDShiftLeft    uint16 = 0xE1
	HIDAltLeft      uint16 = 0xE2
	HIDMetaLeft     uint16 = 0xE3
	HIDControlRight uint16 = 0xE4
	HIDShiftRight   uint16 = 0xE5
	HIDAltRight     uint16 = 0xE6
	HIDMetaRight    uint16 = 0xE7

	// HIDMax is one past the largest usage the bitmap can carry.
	HIDMax uint16 = KeyBitmapBytes * 8
)

// ScanCode is a PS/2 set-1 make code. Values above 0xFF carry the 0xE0 prefix
// in their high byte, which becomes KEYEVENTF_EXTENDEDKEY at injection time.
type ScanCode uint16

// Extended reports whether the code needs the 0xE0 prefix.
func (s ScanCode) Extended() bool { return s&0xFF00 == 0xE000 }

// Byte returns the single-byte make code without any prefix.
func (s ScanCode) Byte() uint8 { return uint8(s & 0xFF) }

// hidToScan maps HID keyboard usage IDs to PS/2 set-1 make codes.
//
// A zero entry means "no direct scancode"; callers must fall back to a
// virtual-key or Unicode injection path for those.
var hidToScan = [HIDMax]ScanCode{
	// Letters, in HID order a..z.
	0x04: 0x1E, 0x05: 0x30, 0x06: 0x2E, 0x07: 0x20, 0x08: 0x12, 0x09: 0x21,
	0x0A: 0x22, 0x0B: 0x23, 0x0C: 0x17, 0x0D: 0x24, 0x0E: 0x25, 0x0F: 0x26,
	0x10: 0x32, 0x11: 0x31, 0x12: 0x18, 0x13: 0x19, 0x14: 0x10, 0x15: 0x13,
	0x16: 0x1F, 0x17: 0x14, 0x18: 0x16, 0x19: 0x2F, 0x1A: 0x11, 0x1B: 0x2D,
	0x1C: 0x15, 0x1D: 0x2C,

	// Digit row 1..9 then 0.
	0x1E: 0x02, 0x1F: 0x03, 0x20: 0x04, 0x21: 0x05, 0x22: 0x06,
	0x23: 0x07, 0x24: 0x08, 0x25: 0x09, 0x26: 0x0A, 0x27: 0x0B,

	0x28: 0x1C, // Enter
	0x29: 0x01, // Escape
	0x2A: 0x0E, // Backspace
	0x2B: 0x0F, // Tab
	0x2C: 0x39, // Space
	0x2D: 0x0C, // Minus
	0x2E: 0x0D, // Equal
	0x2F: 0x1A, // BracketLeft
	0x30: 0x1B, // BracketRight
	0x31: 0x2B, // Backslash
	0x32: 0x2B, // NonUSHash (same physical key on many layouts)
	0x33: 0x27, // Semicolon
	0x34: 0x28, // Quote
	0x35: 0x29, // Backquote
	0x36: 0x33, // Comma
	0x37: 0x34, // Period
	0x38: 0x35, // Slash
	0x39: 0x3A, // CapsLock

	// F1..F12.
	0x3A: 0x3B, 0x3B: 0x3C, 0x3C: 0x3D, 0x3D: 0x3E, 0x3E: 0x3F, 0x3F: 0x40,
	0x40: 0x41, 0x41: 0x42, 0x42: 0x43, 0x43: 0x44, 0x44: 0x57, 0x45: 0x58,

	0x46: 0xE037, // PrintScreen
	0x47: 0x46,   // ScrollLock
	// 0x48 Pause has no single make code (E1 1D 45 …) and is injected by
	// virtual key instead; left zero deliberately.
	0x49: 0xE052, // Insert
	0x4A: 0xE047, // Home
	0x4B: 0xE049, // PageUp
	0x4C: 0xE053, // Delete
	0x4D: 0xE04F, // End
	0x4E: 0xE051, // PageDown
	0x4F: 0xE04D, // ArrowRight
	0x50: 0xE04B, // ArrowLeft
	0x51: 0xE050, // ArrowDown
	0x52: 0xE048, // ArrowUp

	0x53: 0x45,   // NumLock
	0x54: 0xE035, // NumpadDivide
	0x55: 0x37,   // NumpadMultiply
	0x56: 0x4A,   // NumpadSubtract
	0x57: 0x4E,   // NumpadAdd
	0x58: 0xE01C, // NumpadEnter
	0x59: 0x4F, 0x5A: 0x50, 0x5B: 0x51, 0x5C: 0x4B, 0x5D: 0x4C,
	0x5E: 0x4D, 0x5F: 0x47, 0x60: 0x48, 0x61: 0x49,
	0x62: 0x52, // Numpad0
	0x63: 0x53, // NumpadDecimal

	0x64: 0x56,   // IntlBackslash
	0x65: 0xE05D, // ContextMenu
	0x67: 0x59,   // NumpadEqual

	// F13..F24.
	0x68: 0x64, 0x69: 0x65, 0x6A: 0x66, 0x6B: 0x67, 0x6C: 0x68, 0x6D: 0x69,
	0x6E: 0x6A, 0x6F: 0x6B, 0x70: 0x6C, 0x71: 0x6D, 0x72: 0x6E, 0x73: 0x76,

	0x87: 0x73, // IntlRo
	0x88: 0x70, // KanaMode
	0x89: 0x7D, // IntlYen
	0x8A: 0x79, // Convert
	0x8B: 0x7B, // NonConvert
	0x90: 0x72, // Lang1 / HangulMode
	0x91: 0x71, // Lang2 / Hanja

	0xE0: 0x1D,   // ControlLeft
	0xE1: 0x2A,   // ShiftLeft
	0xE2: 0x38,   // AltLeft
	0xE3: 0xE05B, // MetaLeft
	0xE4: 0xE01D, // ControlRight
	0xE5: 0x36,   // ShiftRight
	0xE6: 0xE038, // AltRight
	0xE7: 0xE05C, // MetaRight
}

// ScanCodeFor returns the PS/2 set-1 make code for a HID usage.
// ok is false when the usage has no scancode and needs a virtual-key path.
func ScanCodeFor(usage uint16) (code ScanCode, ok bool) {
	if usage >= HIDMax {
		return 0, false
	}
	code = hidToScan[usage]
	return code, code != 0
}

// IsModifier reports whether a usage is one of the eight modifier keys.
func IsModifier(usage uint16) bool {
	return usage >= HIDControlLeft && usage <= HIDMetaRight
}

// SystemAction names a privileged action the agent can perform on the host.
type SystemAction struct {
	// Action is one of:
	//   "sas"        — Ctrl+Alt+Del. Requires the SYSTEM service; SendInput
	//                  cannot generate it, by Windows design.
	//   "lock"       — lock the workstation.
	//   "displayOn"  — wake the display and defeat the screensaver.
	Action string `json:"action"`
}

// TypeSystemAction is the control message carrying SystemAction.
const TypeSystemAction uint8 = 0xCA
