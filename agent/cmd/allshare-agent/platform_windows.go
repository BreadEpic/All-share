//go:build windows

package main

import (
	"fmt"
	"log/slog"

	"github.com/mmc/all-share/internal/capture"
)

// newDesktopProvider returns the real Windows screen-capture backend.
func newDesktopProvider(log *slog.Logger) (capture.Provider, error) {
	return capture.NewWindowsProvider(log)
}

// cmdPlatform dispatches the Windows-only commands.
func cmdPlatform(command string, args []string) error {
	switch command {
	case "service":
		return cmdService(args)
	case "host":
		return cmdHost(args)
	case "install":
		return cmdInstall(args)
	case "uninstall":
		return cmdUninstall(args)
	case "checkin":
		return cmdCheckIn(args)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
