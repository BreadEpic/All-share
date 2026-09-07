//go:build windows

// Windows input injection for ALL SHARE.
//
// Keys are injected as PS/2 scancodes rather than virtual keys. That choice
// matters more than it looks: scancodes are what DirectInput and Raw Input read,
// so games see real key presses, and Windows applies its own keyboard layout to
// them, so the remote machine behaves exactly as it would under the user's own
// hands. Virtual keys would work for typing in Notepad and fail in every game.
//
// Relative pointer movement is injected with MOUSEEVENTF_MOVE rather than by
// warping the cursor. Warping would defeat Raw Input entirely, which is what a
// first-person game reads; MOUSEEVENTF_MOVE reaches both the cursor and the raw
// input stream, exactly as a physical mouse does.
package input

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/mmc/all-share/shared/protocol"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procGetSystemMet = user32.NewProc("GetSystemMetrics")
	procSetCursorPos = user32.NewProc("SetCursorPos")
	procGetCursorPos = user32.NewProc("GetCursorPos")
	procLockWorkStn  = user32.NewProc("LockWorkStation")
	procGetAsyncKey  = user32.NewProc("GetAsyncKeyState")

	// SendSAS lives in sas.dll and is the only supported way to generate
	// Ctrl+Alt+Delete. SendInput cannot: Windows reserves the secure attention
	// sequence for the kernel and Winlogon precisely so that no program can
	// fake a logon prompt.
	sasDLL      = windows.NewLazySystemDLL("sas.dll")
	procSendSAS = sasDLL.NewProc("SendSAS")

	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecutio = kernel32.NewProc("SetThreadExecutionState")
)

// Windows constants used by SendInput.
const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove        = 0x0001
	mouseEventLeftDown    = 0x0002
	mouseEventLeftUp      = 0x0004
	mouseEventRightDown   = 0x0008
	mouseEventRightUp     = 0x0010
	mouseEventMiddleDown  = 0x0020
	mouseEventMiddleUp    = 0x0040
	mouseEventXDown       = 0x0080
	mouseEventXUp         = 0x0100
	mouseEventWheel       = 0x0800
	mouseEventHWheel      = 0x1000
	mouseEventAbsolute    = 0x8000
	mouseEventVirtualDesk = 0x4000

	xbutton1 = 0x0001
	xbutton2 = 0x0002

	keyEventExtendedKey = 0x0001
	keyEventKeyUp       = 0x0002
	keyEventUnicode     = 0x0004
	keyEventScancode    = 0x0008

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	vkPause = 0x13

	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
	esContinuous      = 0x80000000
)

type mouseInput struct {
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keyboardInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
	// Padding so the union matches the size of MOUSEINPUT, which is what the
	// INPUT structure's layout requires.
	_ [8]byte
}

type inputUnion struct {
	inputType uint32
	_         uint32 // alignment before the union on 64-bit
	data      [24]byte
}

// injector applies remote input to this machine.
type injector struct {
	mu sync.Mutex

	// held tracks what this injector has pressed, so everything can be released
	// when a session ends. A stuck modifier on someone's own PC is both
	// maddening and hard to diagnose, so it is prevented structurally.
	heldKeys    map[uint16]bool
	heldButtons uint8

	relative bool

	// captureRect maps the normalized coordinates the client sends onto the
	// display actually being captured. Without it, a click on a second monitor
	// would land on the primary one.
	captureLeft, captureTop     int32
	captureWidth, captureHeight int32
	haveCaptureRect             bool
}

// New returns an injector for this platform.
func New() (Injector, error) {
	if err := procSendInput.Find(); err != nil {
		return nil, fmt.Errorf("allshare/input: this system has no input injection: %w", err)
	}
	return &injector{heldKeys: map[uint16]bool{}}, nil
}

// SetCaptureRect tells the injector which part of the virtual desktop the
// client is looking at, in physical pixels.
func (i *injector) SetCaptureRect(left, top, width, height int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.captureLeft = int32(left)
	i.captureTop = int32(top)
	i.captureWidth = int32(width)
	i.captureHeight = int32(height)
	i.haveCaptureRect = width > 0 && height > 0
}

func sendInputs(inputs []inputUnion) error {
	if len(inputs) == 0 {
		return nil
	}
	sent, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if int(sent) != len(inputs) {
		// The usual cause is that this process is not on the input desktop —
		// the machine is locked, or a UAC prompt is up on the secure desktop.
		if err != nil && !errors.Is(err, syscall.Errno(0)) {
			return fmt.Errorf("allshare/input: SendInput delivered %d of %d events: %w",
				sent, len(inputs), err)
		}
		return fmt.Errorf("allshare/input: SendInput delivered %d of %d events", sent, len(inputs))
	}
	return nil
}

func mouseInputStruct(m mouseInput) inputUnion {
	var in inputUnion
	in.inputType = inputMouse
	*(*mouseInput)(unsafe.Pointer(&in.data[0])) = m
	return in
}

func keyInputStruct(k keyboardInput) inputUnion {
	var in inputUnion
	in.inputType = inputKeyboard
	*(*keyboardInput)(unsafe.Pointer(&in.data[0])) = k
	return in
}

// MoveAbsolute positions the pointer from a normalized coordinate.
func (i *injector) MoveAbsolute(x, y uint16) {
	i.mu.Lock()
	left, top := i.captureLeft, i.captureTop
	width, height := i.captureWidth, i.captureHeight
	haveRect := i.haveCaptureRect
	i.mu.Unlock()

	// Map the client's normalized position onto the captured display, then onto
	// the whole virtual desktop, which is the coordinate space SendInput uses
	// for absolute movement.
	var virtualX, virtualY int32
	if haveRect {
		virtualX = left + int32((int64(x)*int64(width-1))/65535)
		virtualY = top + int32((int64(y)*int64(height-1))/65535)
	} else {
		primaryW, _, _ := procGetSystemMet.Call(uintptr(smCXVirtualScreen))
		primaryH, _, _ := procGetSystemMet.Call(uintptr(smCYVirtualScreen))
		virtualX = int32((int64(x) * int64(int32(primaryW)-1)) / 65535)
		virtualY = int32((int64(y) * int64(int32(primaryH)-1)) / 65535)
	}

	originX, _, _ := procGetSystemMet.Call(uintptr(smXVirtualScreen))
	originY, _, _ := procGetSystemMet.Call(uintptr(smYVirtualScreen))
	spanX, _, _ := procGetSystemMet.Call(uintptr(smCXVirtualScreen))
	spanY, _, _ := procGetSystemMet.Call(uintptr(smCYVirtualScreen))
	if int32(spanX) <= 1 || int32(spanY) <= 1 {
		return
	}

	// SendInput's absolute coordinates are themselves normalized to 0..65535
	// across the virtual desktop, so this converts once more.
	normX := ((int64(virtualX) - int64(int32(originX))) * 65535) / int64(int32(spanX)-1)
	normY := ((int64(virtualY) - int64(int32(originY))) * 65535) / int64(int32(spanY)-1)

	_ = sendInputs([]inputUnion{mouseInputStruct(mouseInput{
		dx:      int32(clampInt64(normX, 0, 65535)),
		dy:      int32(clampInt64(normY, 0, 65535)),
		dwFlags: mouseEventMove | mouseEventAbsolute | mouseEventVirtualDesk,
	})})
}

// MoveRelative applies a raw delta, as a physical mouse would.
func (i *injector) MoveRelative(dx, dy int) {
	if dx == 0 && dy == 0 {
		return
	}
	_ = sendInputs([]inputUnion{mouseInputStruct(mouseInput{
		dx:      int32(dx),
		dy:      int32(dy),
		dwFlags: mouseEventMove,
	})})
}

// MouseButton presses or releases a button.
func (i *injector) MouseButton(index uint8, down bool) {
	var flags uint32
	var data uint32
	switch index {
	case 0:
		flags = mouseEventLeftDown
		if !down {
			flags = mouseEventLeftUp
		}
	case 1:
		flags = mouseEventRightDown
		if !down {
			flags = mouseEventRightUp
		}
	case 2:
		flags = mouseEventMiddleDown
		if !down {
			flags = mouseEventMiddleUp
		}
	case 3, 4:
		flags = mouseEventXDown
		if !down {
			flags = mouseEventXUp
		}
		data = xbutton1
		if index == 4 {
			data = xbutton2
		}
	default:
		return
	}

	i.mu.Lock()
	bit := uint8(1) << index
	if down {
		i.heldButtons |= bit
	} else {
		i.heldButtons &^= bit
	}
	i.mu.Unlock()

	_ = sendInputs([]inputUnion{mouseInputStruct(mouseInput{
		mouseData: data,
		dwFlags:   flags,
	})})
}

// Wheel scrolls in 1/120 notch units, which is what Windows itself uses.
func (i *injector) Wheel(dx, dy int) {
	var inputs []inputUnion
	if dy != 0 {
		inputs = append(inputs, mouseInputStruct(mouseInput{
			mouseData: uint32(int32(dy)),
			dwFlags:   mouseEventWheel,
		}))
	}
	if dx != 0 {
		inputs = append(inputs, mouseInputStruct(mouseInput{
			mouseData: uint32(int32(dx)),
			dwFlags:   mouseEventHWheel,
		}))
	}
	_ = sendInputs(inputs)
}

// Key presses or releases a key identified by its USB HID usage.
func (i *injector) Key(usage uint16, down bool) {
	scan, ok := protocol.ScanCodeFor(usage)
	if !ok {
		// Pause has no single make code — its scancode sequence is a special
		// case in the PS/2 protocol — so it goes through a virtual key.
		if usage == protocol.HIDPause {
			flags := uint32(0)
			if !down {
				flags = keyEventKeyUp
			}
			_ = sendInputs([]inputUnion{keyInputStruct(keyboardInput{wVk: vkPause, dwFlags: flags})})
		}
		return
	}

	i.mu.Lock()
	if down {
		i.heldKeys[usage] = true
	} else {
		delete(i.heldKeys, usage)
	}
	i.mu.Unlock()

	flags := uint32(keyEventScancode)
	if scan.Extended() {
		flags |= keyEventExtendedKey
	}
	if !down {
		flags |= keyEventKeyUp
	}
	_ = sendInputs([]inputUnion{keyInputStruct(keyboardInput{
		wScan:   uint16(scan.Byte()),
		dwFlags: flags,
	})})
}

// TypeText enters literal text, used by the layout-matching mode and by paste.
func (i *injector) TypeText(text string) {
	if text == "" {
		return
	}
	// UTF-16 with surrogate pairs sent as two separate events, which is what
	// KEYEVENTF_UNICODE expects for anything outside the basic plane.
	units := windows.StringToUTF16(text)
	if len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}

	const batch = 64
	for start := 0; start < len(units); start += batch {
		end := start + batch
		if end > len(units) {
			end = len(units)
		}
		inputs := make([]inputUnion, 0, (end-start)*2)
		for _, unit := range units[start:end] {
			inputs = append(inputs,
				keyInputStruct(keyboardInput{wScan: unit, dwFlags: keyEventUnicode}),
				keyInputStruct(keyboardInput{wScan: unit, dwFlags: keyEventUnicode | keyEventKeyUp}))
		}
		if err := sendInputs(inputs); err != nil {
			return
		}
	}
}

// ReleaseAll lets go of everything this injector has pressed.
func (i *injector) ReleaseAll() {
	i.mu.Lock()
	keys := make([]uint16, 0, len(i.heldKeys))
	for usage := range i.heldKeys {
		keys = append(keys, usage)
	}
	buttons := i.heldButtons
	i.heldKeys = map[uint16]bool{}
	i.heldButtons = 0
	i.mu.Unlock()

	var inputs []inputUnion
	for _, usage := range keys {
		scan, ok := protocol.ScanCodeFor(usage)
		if !ok {
			continue
		}
		flags := uint32(keyEventScancode | keyEventKeyUp)
		if scan.Extended() {
			flags |= keyEventExtendedKey
		}
		inputs = append(inputs, keyInputStruct(keyboardInput{
			wScan: uint16(scan.Byte()), dwFlags: flags,
		}))
	}
	for index := uint8(0); index < 5; index++ {
		if buttons&(1<<index) == 0 {
			continue
		}
		var flags, data uint32
		switch index {
		case 0:
			flags = mouseEventLeftUp
		case 1:
			flags = mouseEventRightUp
		case 2:
			flags = mouseEventMiddleUp
		case 3:
			flags, data = mouseEventXUp, xbutton1
		case 4:
			flags, data = mouseEventXUp, xbutton2
		}
		inputs = append(inputs, mouseInputStruct(mouseInput{mouseData: data, dwFlags: flags}))
	}
	_ = sendInputs(inputs)
}

// SetPointerMode records whether the client is sending relative positions.
func (i *injector) SetPointerMode(relative bool) {
	i.mu.Lock()
	i.relative = relative
	i.mu.Unlock()
}

// SystemAction performs a privileged host action.
func (i *injector) SystemAction(action string) error {
	switch action {
	case "sas":
		// Ctrl+Alt+Delete. Requires either the SYSTEM service or the
		// SoftwareSASGeneration policy; SendInput can never produce it, by
		// design, because Windows reserves the sequence so no program can fake
		// a logon screen.
		if err := procSendSAS.Find(); err != nil {
			return fmt.Errorf("allshare/input: this edition of Windows does not offer SendSAS: %w", err)
		}
		ret, _, err := procSendSAS.Call(0)
		if ret == 0 {
			return fmt.Errorf("allshare/input: Windows refused Ctrl+Alt+Delete: %w", err)
		}
		return nil

	case "lock":
		ret, _, err := procLockWorkStn.Call()
		if ret == 0 {
			return fmt.Errorf("allshare/input: could not lock this PC: %w", err)
		}
		return nil

	case "displayOn":
		// Waking the display and defeating the screensaver, so a remote user is
		// not looking at a black rectangle.
		procSetThreadExecutio.Call(uintptr(esDisplayRequired | esSystemRequired))
		procSetThreadExecutio.Call(uintptr(esContinuous))
		return nil

	default:
		return fmt.Errorf("%w: %q", ErrUnsupported, action)
	}
}

// Close releases anything held.
func (i *injector) Close() error {
	i.ReleaseAll()
	return nil
}

// HeldKeyCount reports how many keys this injector believes are down, for
// diagnostics.
func (i *injector) HeldKeyCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.heldKeys)
}

func clampInt64(v, low, high int64) int64 {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
