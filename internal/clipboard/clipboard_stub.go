//go:build !windows

package clipboard

// New returns the clipboard for this platform.
//
// Only Windows is implemented; elsewhere the agent runs without clipboard
// sharing rather than failing to start.
func New() (Clipboard, error) { return Noop{}, nil }
