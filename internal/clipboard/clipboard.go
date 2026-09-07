// Package clipboard shares text between the PC and a connected client.
//
// Only plain text moves, and only in the directions the user has enabled. That
// is a deliberate limit: a clipboard can hold passwords, and silently streaming
// whatever a user copies to a remote device is not a feature, it is a leak.
package clipboard

import (
	"errors"
	"sync"
)

// ErrUnsupported reports that this platform has no clipboard integration.
var ErrUnsupported = errors.New("allshare/clipboard: not supported on this system")

// MaxBytes bounds a transfer in either direction.
const MaxBytes = 64 * 1024

// Clipboard is the host clipboard.
type Clipboard interface {
	Read() (string, error)
	Write(text string) error
	// Changes emits text copied on this PC. Nil when the platform cannot watch.
	Changes() <-chan string
	Close() error
}

// Noop is a clipboard that does nothing, used where the platform has none.
type Noop struct{}

// Read always reports that there is nothing to read.
func (Noop) Read() (string, error) { return "", ErrUnsupported }

// Write discards the text.
func (Noop) Write(string) error { return ErrUnsupported }

// Changes returns nil, so nothing watches it.
func (Noop) Changes() <-chan string { return nil }

// Close does nothing.
func (Noop) Close() error { return nil }

// Recorder is a Clipboard that records what it was asked to do instead of
// touching a machine. It backs the end-to-end tests, which need to prove that
// text copied in the browser really crossed the network and arrived intact —
// and, just as importantly, that oversized text did not arrive truncated.
type Recorder struct {
	mu      sync.Mutex
	written []string
	changes chan string
	closed  bool
}

// NewRecorder builds a recording clipboard.
func NewRecorder() *Recorder {
	return &Recorder{changes: make(chan string, 16)}
}

// Read reports the most recent text written, or nothing.
func (r *Recorder) Read() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.written) == 0 {
		return "", nil
	}
	return r.written[len(r.written)-1], nil
}

// Write records the text.
func (r *Recorder) Write(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.written = append(r.written, text)
	return nil
}

// Written returns every text written so far.
func (r *Recorder) Written() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.written))
	copy(out, r.written)
	return out
}

// Copy simulates the user copying text on the PC.
func (r *Recorder) Copy(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.changes <- text:
	default:
	}
}

// Changes emits text "copied" on this PC.
func (r *Recorder) Changes() <-chan string { return r.changes }

// Close stops the change stream.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.changes)
	}
	return nil
}
