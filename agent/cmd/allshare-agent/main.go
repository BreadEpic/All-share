// Command allshare-agent is the ALL SHARE PC-side program.
//
// It runs in one of several modes:
//
//	run        the agent in the foreground, for setup and troubleshooting
//	service    the agent as a Windows service (the normal installed mode)
//	host       the desktop-side worker the service launches into the user's
//	           session, which is what makes capture and input work at the lock
//	           screen and across user switches
//	pair       print a pairing code and wait for a device
//	status     report what the installed agent is doing
//
// The service/host split exists because Windows isolates services in session 0,
// where there is no desktop to capture and no input queue to inject into. The
// service owns the network and the identity; the host owns the desktop. That is
// also what keeps a session alive across a lock, a user switch, or a UAC prompt.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mmc/all-share/internal/agentcore"
	"github.com/mmc/all-share/internal/capture"
	"github.com/mmc/all-share/internal/capture/testsource"
	"github.com/mmc/all-share/internal/clipboard"
	"github.com/mmc/all-share/internal/config"
	agentinput "github.com/mmc/all-share/internal/input"
	"github.com/mmc/all-share/internal/wake"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/pair"
	"github.com/mmc/all-share/shared/rvclient"
)

// Version is stamped at build time with -ldflags "-X main.Version=…".
var Version = "dev"

func main() {
	agentcore.Version = Version

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	args := os.Args[2:]

	var err error
	switch command {
	case "run":
		err = cmdRun(args)
	case "pair":
		err = cmdPair(args)
	case "status":
		err = cmdStatus(args)
	case "forget", "unpair":
		err = cmdForget(args)
	case "config":
		err = cmdConfig(args)
	case "service", "host", "install", "uninstall":
		err = cmdPlatform(command, args)
	case "version", "--version", "-v":
		fmt.Println("ALL SHARE agent", Version, "("+runtime.GOOS+"/"+runtime.GOARCH+")")
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "allshare-agent:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ALL SHARE agent — remote access for this PC

Usage:
  allshare-agent run          Run in the foreground (setup and troubleshooting)
  allshare-agent pair         Show a pairing code and wait for a device
  allshare-agent status       Show what this PC is doing
  allshare-agent forget       Remove a paired device from this PC
  allshare-agent config       Show or change settings
  allshare-agent service      Run as a Windows service
  allshare-agent host         Run the desktop worker (started by the service)
  allshare-agent install      Install the Windows service
  allshare-agent uninstall    Remove the Windows service
  allshare-agent version      Print the version

Run "allshare-agent <command> -h" for the options of a command.
`)
}

// runtimeParts is everything a running agent needs, assembled once.
type runtimeParts struct {
	cfg       *config.Config
	identity  idkey.PrivateKey
	provider  capture.Provider
	injector  agentinput.Injector
	clipboard clipboard.Clipboard
	wake      *wake.Manager
	log       *slog.Logger
}

func assemble(dataDir, captureMode, logLevel string) (*runtimeParts, error) {
	log := newLogger(logLevel)
	slog.SetDefault(log)

	if dataDir == "" {
		dir, err := config.Dir()
		if err != nil {
			return nil, err
		}
		dataDir = dir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	cfg, err := config.Load(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	cfg.SetPath(filepath.Join(dataDir, "config.json"))

	identity, created, err := idkey.LoadOrCreate(filepath.Join(dataDir, "device-identity.key"))
	if err != nil {
		return nil, fmt.Errorf("load this PC's identity: %w", err)
	}
	if created {
		log.Info("created this PC's identity", "fingerprint", identity.Public().Fingerprint())
	}

	provider, err := openProvider(captureMode, log)
	if err != nil {
		return nil, err
	}

	injector, err := agentinput.New()
	if err != nil {
		log.Warn("input control is unavailable on this system", "err", err)
		injector = agentinput.NewRecorder(64)
	}

	wakeManager := wake.NewManager(wake.NewPlatform(), cfg.WakeCheckInMinutes, cfg.WakeEnabled)
	if err := wakeManager.Apply(); err != nil {
		log.Debug("scheduled wake could not be armed", "err", err)
	}

	board, err := clipboard.New()
	if err != nil {
		// Clipboard sharing is a convenience; losing it must not stop a session.
		log.Info("clipboard sharing is unavailable on this system", "err", err)
		board = clipboard.Noop{}
	}

	return &runtimeParts{
		cfg: cfg, identity: identity, provider: provider,
		injector: injector, clipboard: board, wake: wakeManager, log: log,
	}, nil
}

// openProvider selects a capture backend.
//
// "auto" prefers the platform backend and falls back to the test pattern, which
// is what lets the agent run — and be exercised end to end — on a machine with
// no desktop at all.
func openProvider(mode string, log *slog.Logger) (capture.Provider, error) {
	switch mode {
	case "test":
		return testsource.New()
	case "desktop":
		provider, err := newDesktopProvider(log)
		if err != nil {
			return nil, fmt.Errorf("start screen capture: %w", err)
		}
		return provider, nil
	default:
		provider, err := newDesktopProvider(log)
		if err == nil {
			return provider, nil
		}
		log.Warn("screen capture is unavailable; using the test pattern instead", "err", err)
		return testsource.New()
	}
}

func cmdRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	url := flags.String("service", "", "ALL SHARE service address (overrides the saved setting)")
	captureMode := flags.String("capture", "auto", "capture backend: auto, desktop or test")
	logLevel := flags.String("log-level", "", "debug, info, warn or error")
	pairNow := flags.Bool("pair", false, "show a pairing code as soon as the agent connects")
	// Set when the Windows service starts this process inside the interactive
	// session. It only changes presentation: no banner, and logs go to stderr
	// for the service to collect.
	supervised := flags.Bool("supervised", false, "started by the ALL SHARE service")
	if err := flags.Parse(args); err != nil {
		return err
	}

	parts, err := assemble(*dataDir, *captureMode, firstNonEmpty(*logLevel, "info"))
	if err != nil {
		return err
	}
	if *url != "" {
		if err := parts.cfg.Update(func(c *config.Config) { c.Rendezvous = *url }); err != nil {
			return err
		}
	}
	if parts.cfg.Rendezvous == "" {
		return errors.New("no ALL SHARE service address is set. Run: allshare-agent config -service wss://your-service/rv")
	}

	instance, err := agentcore.New(agentcore.Options{
		Config: parts.cfg, Identity: parts.identity,
		Provider: parts.provider, Injector: parts.injector,
		Clipboard: parts.clipboard, Wake: parts.wake, Log: parts.log,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go printEvents(instance)
	if *pairNow {
		go func() {
			// Give the rendezvous a moment to connect so the code is usable the
			// instant it appears, rather than failing on the first attempt.
			time.Sleep(1500 * time.Millisecond)
			code, expires, err := instance.BeginPairing()
			if err != nil {
				fmt.Fprintln(os.Stderr, "could not start pairing:", err)
				return
			}
			printPairingCode(parts.cfg.DeviceName, parts.cfg.Rendezvous, code, expires)
		}()
	}

	if !*supervised {
		fmt.Printf("ALL SHARE is running on %q\n", parts.cfg.DeviceName)
		fmt.Printf("  This PC's identity: %s\n", parts.identity.Public().Fingerprint())
		fmt.Printf("  Service:            %s\n", parts.cfg.Rendezvous)
		fmt.Printf("  Screen capture:     %s\n", parts.provider.Name())
		fmt.Printf("  Paired devices:     %d\n\n", len(parts.cfg.PairedIDs()))
	}

	err = instance.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func cmdPair(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	captureMode := flags.String("capture", "auto", "capture backend: auto, desktop or test")
	if err := flags.Parse(args); err != nil {
		return err
	}

	parts, err := assemble(*dataDir, *captureMode, "warn")
	if err != nil {
		return err
	}
	if parts.cfg.Rendezvous == "" {
		return errors.New("no ALL SHARE service address is set. Run: allshare-agent config -service wss://your-service/rv")
	}

	instance, err := agentcore.New(agentcore.Options{
		Config: parts.cfg, Identity: parts.identity,
		Provider: parts.provider, Injector: parts.injector,
		Clipboard: parts.clipboard, Wake: parts.wake, Log: parts.log,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()

	// Wait for the connection before showing a code, so the user is never
	// looking at a code that cannot possibly work yet.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if instance.Status().Connected {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !instance.Status().Connected {
		return errors.New("could not reach the ALL SHARE service. Check the address and this PC's internet connection")
	}

	code, expires, err := instance.BeginPairing()
	if err != nil {
		return err
	}
	printPairingCode(parts.cfg.DeviceName, parts.cfg.Rendezvous, code, expires)

	events := instance.Events()
	timeout := time.NewTimer(time.Until(expires))
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case <-timeout.C:
			instance.CancelPairing()
			return errors.New("the code expired before a device used it. Run this again for a new code")
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.Kind == agentcore.EventPairingDone {
				fmt.Printf("\n  Paired with %s.\n  You can now connect from that device.\n", event.Label)
				return nil
			}
		}
	}
}

func printPairingCode(deviceName, service, code string, expires time.Time) {
	line := strings.Repeat("─", 46)
	fmt.Printf("\n  ┌%s┐\n", line)
	fmt.Printf("  │%s│\n", center("ADD A DEVICE TO "+strings.ToUpper(deviceName), 46))
	fmt.Printf("  │%s│\n", center("", 46))
	fmt.Printf("  │%s│\n", center(pair.FormatCode(code), 46))
	fmt.Printf("  │%s│\n", center("", 46))
	fmt.Printf("  │%s│\n", center("Service: "+shortenService(service), 46))
	fmt.Printf("  └%s┘\n\n", line)
	fmt.Printf("  On your other device, open ALL SHARE, choose \"Add a PC\",\n")
	fmt.Printf("  and enter this code. It expires in %s.\n\n", time.Until(expires).Round(time.Second))
}

func center(text string, width int) string {
	runes := []rune(text)
	if len(runes) >= width {
		return string(runes[:width])
	}
	left := (width - len(runes)) / 2
	right := width - len(runes) - left
	return strings.Repeat(" ", left) + text + strings.Repeat(" ", right)
}

func shortenService(service string) string {
	if len(service) <= 30 {
		return service
	}
	return service[:27] + "…"
}

func cmdStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	if err := flags.Parse(args); err != nil {
		return err
	}

	dir := *dataDir
	if dir == "" {
		resolved, err := config.Dir()
		if err != nil {
			return err
		}
		dir = resolved
	}
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	identity, err := idkey.Load(filepath.Join(dir, "device-identity.key"))
	fingerprint := "not set up yet"
	if err == nil {
		fingerprint = identity.Public().Fingerprint()
	}

	fmt.Printf("ALL SHARE agent %s\n\n", Version)
	fmt.Printf("  PC name:         %s\n", cfg.DeviceName)
	fmt.Printf("  Identity:        %s\n", fingerprint)
	fmt.Printf("  Service:         %s\n", orNone(cfg.Rendezvous))
	fmt.Printf("  Paired devices:  %d\n", len(cfg.PairedClients))
	for _, client := range cfg.PairedClients {
		last := "never connected"
		if !client.LastSeen.IsZero() {
			last = "last connected " + client.LastSeen.Local().Format("2 Jan 15:04")
		}
		fmt.Printf("      • %-24s %-14s %s\n", client.Label, clientFingerprint(client.ID), last)
	}
	if len(cfg.PairedClients) > 0 {
		fmt.Printf("\n  To remove one:   allshare-agent forget <name or code>\n")
	}
	fmt.Printf("  Settings file:   %s\n", cfg.Path())
	fmt.Printf("  Wake check-in:   %s\n", describeCheckIn(cfg))
	return nil
}

func describeCheckIn(cfg *config.Config) string {
	if !cfg.WakeEnabled {
		return "off"
	}
	if cfg.WakeCheckInMinutes <= 0 {
		return "off"
	}
	return fmt.Sprintf("every %d minutes", cfg.WakeCheckInMinutes)
}

// clientFingerprint renders a paired client's key the way its own screen does,
// so a user can match the two by eye.
func clientFingerprint(id string) string {
	pub, err := idkey.ParsePublic(id)
	if err != nil {
		return "unreadable"
	}
	return pub.Fingerprint()
}

// cmdForget removes a paired device from this PC.
//
// This is the recovery path for a lost or stolen device, so it has to work from
// the PC and only from the PC. Removing a device through the client would be
// useless in exactly the case that matters: the thief has the client.
//
// The removal is final and needs no server. The agent checks its own paired
// list on every connection request, so a forgotten device is refused even if
// the rendezvous still believes in it.
func cmdForget(args []string) error {
	flags := flag.NewFlagSet("forget", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	all := flags.Bool("all", false, "remove every paired device")
	if err := flags.Parse(args); err != nil {
		return err
	}

	dir := *dataDir
	if dir == "" {
		resolved, err := config.Dir()
		if err != nil {
			return err
		}
		dir = resolved
	}
	path := filepath.Join(dir, "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.SetPath(path)

	clients := cfg.PairedClients
	if len(clients) == 0 {
		fmt.Println("This PC is not paired with any devices.")
		return nil
	}

	if *all {
		for _, client := range clients {
			if err := cfg.RemovePairedClient(client.ID); err != nil {
				return err
			}
		}
		fmt.Printf("Removed all %d devices. None of them can connect to this PC any more.\n", len(clients))
		return nil
	}

	target := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if target == "" {
		fmt.Println("Which device? This PC is paired with:")
		fmt.Println()
		for _, client := range clients {
			fmt.Printf("  %-24s %s\n", client.Label, clientFingerprint(client.ID))
		}
		fmt.Println()
		fmt.Println("Run: allshare-agent forget \"<name>\"   (or the code beside it)")
		fmt.Println("     allshare-agent forget -all")
		return nil
	}

	// Match on the name or the fingerprint, either as the user sees it or with
	// the dashes left out, because that is how it will be typed.
	normalized := strings.ToUpper(strings.ReplaceAll(target, "-", ""))
	var matches []config.PairedClient
	for _, client := range clients {
		fingerprint := strings.ReplaceAll(clientFingerprint(client.ID), "-", "")
		if strings.EqualFold(client.Label, target) || fingerprint == normalized {
			matches = append(matches, client)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no paired device is called %q. Run \"allshare-agent forget\" to see the list", target)
	}
	// Two devices can share a label — "Chromebook" twice is entirely normal —
	// and removing the wrong one silently would be worse than refusing.
	if len(matches) > 1 {
		fmt.Printf("More than one device is called %q:\n\n", target)
		for _, client := range matches {
			fmt.Printf("  %-24s %s\n", client.Label, clientFingerprint(client.ID))
		}
		fmt.Println("\nRun the command again with the code instead of the name.")
		return nil
	}

	if err := cfg.RemovePairedClient(matches[0].ID); err != nil {
		return err
	}
	fmt.Printf("Removed %s (%s). It can no longer connect to this PC.\n",
		matches[0].Label, clientFingerprint(matches[0].ID))
	fmt.Println("Pair it again at any time with: allshare-agent pair")
	return nil
}

func cmdConfig(args []string) error {
	flags := flag.NewFlagSet("config", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	service := flags.String("service", "", "set the ALL SHARE service address")
	name := flags.String("name", "", "set the name shown for this PC")
	checkIn := flags.Int("wake-check-in", -1, "wake check-in interval in minutes (0 turns it off)")
	maxFPS := flags.Int("max-fps", 0, "maximum frame rate")
	maxKbps := flags.Int("max-kbps", 0, "maximum bitrate in kbps")
	audio := flags.String("audio", "", "on or off")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// Reject an unencrypted service address here, where the person setting it
	// is watching, rather than at first connect where the message would be
	// buried in a service log.
	if *service != "" {
		if err := rvclient.ValidateEndpoint(*service); err != nil {
			return err
		}
	}

	dir := *dataDir
	if dir == "" {
		resolved, err := config.Dir()
		if err != nil {
			return err
		}
		dir = resolved
	}
	path := filepath.Join(dir, "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.SetPath(path)

	changed := false
	err = cfg.Update(func(c *config.Config) {
		if *service != "" {
			c.Rendezvous = *service
			changed = true
		}
		if *name != "" {
			c.DeviceName = *name
			changed = true
		}
		if *checkIn >= 0 {
			c.WakeCheckInMinutes = *checkIn
			c.WakeEnabled = *checkIn > 0
			changed = true
		}
		if *maxFPS > 0 {
			c.MaxFPS = *maxFPS
			changed = true
		}
		if *maxKbps > 0 {
			c.MaxBitrateKbps = *maxKbps
			changed = true
		}
		switch strings.ToLower(*audio) {
		case "on", "true", "yes":
			c.AudioEnabled = true
			changed = true
		case "off", "false", "no":
			c.AudioEnabled = false
			changed = true
		}
	})
	if err != nil {
		return err
	}
	if !changed {
		return cmdStatus([]string{"-data", dir})
	}
	fmt.Println("Settings saved to", path)
	return cmdStatus([]string{"-data", dir})
}

func printEvents(instance *agentcore.Agent) {
	for event := range instance.Events() {
		switch event.Kind {
		case agentcore.EventSessionStart, agentcore.EventSessionEnd, agentcore.EventPairingDone:
			fmt.Printf("  %s  %s\n", event.At.Format("15:04:05"), event.Message)
		case agentcore.EventError:
			fmt.Printf("  %s  %s (%s)\n", event.At.Format("15:04:05"), event.Message, event.Detail)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func orNone(value string) string {
	if value == "" {
		return "not set"
	}
	return value
}
