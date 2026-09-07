//go:build windows

package clipboard

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	procOpenClipboard     = user32.NewProc("OpenClipboard")
	procCloseClipboard    = user32.NewProc("CloseClipboard")
	procEmptyClipboard    = user32.NewProc("EmptyClipboard")
	procGetClipboardData  = user32.NewProc("GetClipboardData")
	procSetClipboardData  = user32.NewProc("SetClipboardData")
	procGetClipboardSeq   = user32.NewProc("GetClipboardSequenceNumber")
	procIsClipboardFormat = user32.NewProc("IsClipboardFormatAvailable")

	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// lockedText views memory returned by GlobalLock as UTF-16.
//
// Converting a uintptr to a pointer is normally unsound, because Go's garbage
// collector may move the object it refers to. It is sound here, and only here,
// because the address comes from the Windows global heap: that memory is not
// managed by Go, cannot move, and stays valid until GlobalUnlock. Both Win32
// pointer conversions in this package are funnelled through these two helpers
// so there is one place to check that reasoning, and `go vet -unsafeptr=false`
// is used for the Windows cross-build because the analyser cannot express it.
func lockedText(address uintptr) string {
	if address == 0 {
		return ""
	}
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(address))) //nolint:govet
}

// writeLocked copies UTF-16 units into memory returned by GlobalLock.
func writeLocked(address uintptr, units []uint16) {
	if address == 0 || len(units) == 0 {
		return
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(address)), len(units)) //nolint:govet
	copy(dst, units)
}

// Windows clipboard watching.
//
// There is a clipboard-listener window message, but it needs a message pump and
// a window, and a service-launched helper may not have either. Polling the
// sequence number is a single cheap call and misses nothing that matters at a
// human's copying speed.
const pollInterval = 300 * time.Millisecond

type windowsClipboard struct {
	changes chan string
	done    chan struct{}
	once    sync.Once

	mu       sync.Mutex
	lastSeq  uint32
	lastText string
}

// New returns the clipboard for this platform.
func New() (Clipboard, error) {
	if err := procOpenClipboard.Find(); err != nil {
		return Noop{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	c := &windowsClipboard{
		changes: make(chan string, 4),
		done:    make(chan struct{}),
	}
	go c.watch()
	return c, nil
}

// open takes the clipboard, retrying briefly.
//
// The clipboard is a single global resource and another program may hold it for
// a few milliseconds at a time. Failing on the first attempt would make
// clipboard sharing feel randomly broken.
func openClipboard() error {
	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		ret, _, err := procOpenClipboard.Call(0)
		if ret != 0 {
			return nil
		}
		lastErr = err
		time.Sleep(15 * time.Millisecond)
	}
	return fmt.Errorf("allshare/clipboard: another program is holding the clipboard: %w", lastErr)
}

func (c *windowsClipboard) Read() (string, error) {
	available, _, _ := procIsClipboardFormat.Call(uintptr(cfUnicodeText))
	if available == 0 {
		// Not an error: the clipboard simply holds something that is not text,
		// such as an image, and only text is shared.
		return "", nil
	}
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()

	handle, _, err := procGetClipboardData.Call(uintptr(cfUnicodeText))
	if handle == 0 {
		return "", fmt.Errorf("allshare/clipboard: could not read the clipboard: %w", err)
	}
	pointer, _, err := procGlobalLock.Call(handle)
	if pointer == 0 {
		return "", fmt.Errorf("allshare/clipboard: could not read the clipboard: %w", err)
	}
	defer procGlobalUnlock.Call(handle)

	// Bounded so a hostile or enormous clipboard cannot be copied wholesale.
	const maxUnits = MaxBytes / 2
	text := lockedText(pointer)
	if len(text) > maxUnits {
		text = text[:maxUnits]
	}
	return text, nil
}

func (c *windowsClipboard) Write(text string) error {
	if len(text) > MaxBytes {
		text = text[:MaxBytes]
	}
	units, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("allshare/clipboard: that text cannot be placed on the clipboard: %w", err)
	}
	size := uintptr(len(units) * 2)

	handle, _, err := procGlobalAlloc.Call(uintptr(gmemMoveable), size)
	if handle == 0 {
		return fmt.Errorf("allshare/clipboard: out of memory: %w", err)
	}
	pointer, _, _ := procGlobalLock.Call(handle)
	if pointer == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("allshare/clipboard: out of memory")
	}
	writeLocked(pointer, units)
	procGlobalUnlock.Call(handle)

	if err := openClipboard(); err != nil {
		procGlobalFree.Call(handle)
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	if ret, _, err := procSetClipboardData.Call(uintptr(cfUnicodeText), handle); ret == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("allshare/clipboard: could not set the clipboard: %w", err)
	}
	// Ownership passed to the clipboard; freeing it now would corrupt it.

	c.mu.Lock()
	c.lastText = text
	seq, _, _ := procGetClipboardSeq.Call()
	c.lastSeq = uint32(seq)
	c.mu.Unlock()
	return nil
}

func (c *windowsClipboard) watch() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}

		seq, _, _ := procGetClipboardSeq.Call()
		c.mu.Lock()
		unchanged := uint32(seq) == c.lastSeq
		c.mu.Unlock()
		if unchanged {
			continue
		}

		text, err := c.Read()
		c.mu.Lock()
		c.lastSeq = uint32(seq)
		// Text this side just wrote must not be echoed back, or the two
		// clipboards would ping-pong forever.
		echo := text == c.lastText
		c.lastText = text
		c.mu.Unlock()

		if err != nil || text == "" || echo {
			continue
		}
		select {
		case c.changes <- text:
		default:
		}
	}
}

func (c *windowsClipboard) Changes() <-chan string { return c.changes }

func (c *windowsClipboard) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}
