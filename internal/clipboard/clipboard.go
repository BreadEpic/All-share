// Package clipboard shares text between the PC and a connected client.
//
// Only plain text moves, and only in the directions the user has enabled. That
// is a deliberate limit: a clipboard can hold passwords, and silently streaming
// whatever a user copies to a remote device is not a feature, it is a leak.
package clipboard

import "errors"

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
