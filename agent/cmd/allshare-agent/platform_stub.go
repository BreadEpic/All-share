//go:build !windows

package main

import (
	"fmt"
	"log/slog"

	"github.com/mmc/all-share/internal/capture"
)

// newDesktopProvider returns the platform screen-capture backend.
//
// Only Windows has one. Elsewhere the agent still runs with the test pattern,
// which is what makes the whole system testable end to end without a desktop.
func newDesktopProvider(*slog.Logger) (capture.Provider, error) {
	return nil, fmt.Errorf("screen capture is only implemented on Windows")
}

// cmdPlatform handles the Windows-only service commands.
func cmdPlatform(command string, _ []string) error {
	return fmt.Errorf("%q is only available on Windows", command)
}
