//go:build windows

// The ALL SHARE Windows service.
//
// Windows isolates services in session 0, where there is no desktop to capture
// and no input queue to inject into. Anything that touches the screen has to run
// inside the interactive session, on whichever desktop currently has input.
//
// The design here is deliberately a supervisor rather than a split brain. The
// service does one job: keep exactly one agent process running in the active
// session, on the current input desktop, restarting it when Windows switches
// desktops — at a lock, an unlock, a UAC prompt or a fast user switch.
// Everything else, including the rendezvous connection and the whole WebRTC
// stack, lives in that one process.
//
// The alternative — service owns the network, helper owns the desktop, frames
// crossing a named pipe between them — keeps a session alive across a desktop
// switch. It also doubles the moving parts, adds an IPC protocol carrying
// megabits per second, and puts a second failure mode between the user and
// their PC. The supervisor costs a one-to-two second reconnect when the desktop
// changes, which the client already handles automatically because reconnection
// is a first-class path rather than an afterthought. For a product whose second
// priority is reliability, fewer parts that are already exercised beats more
// parts that are not.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/mmc/all-share/internal/config"
)

// ServiceName is how Windows knows the service.
const ServiceName = "ALLShare"

// ServiceDisplayName is what a user sees in the Services list.
const ServiceDisplayName = "ALL SHARE Remote Access"

const serviceDescription = "Lets you reach this PC from your other devices with ALL SHARE. " +
	"Powered by MMC."

var (
	wtsapi32                  = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSGetActiveConsoleID = windows.NewLazySystemDLL("kernel32.dll").NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken     = wtsapi32.NewProc("WTSQueryUserToken")

	userenv             = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvBlock  = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvBlock = userenv.NewProc("DestroyEnvironmentBlock")
)

const (
	createUnicodeEnvironment = 0x00000400
	createNoWindow           = 0x08000000
	createNewConsole         = 0x00000010
)

// ---------------------------------------------------------------------------
// Service entry point
// ---------------------------------------------------------------------------

type allShareService struct {
	log *slog.Logger
}

func cmdService(args []string) error {
	flags := flag.NewFlagSet("service", flag.ExitOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}

	interactive, err := svc.IsAnInteractiveSession()
	if err != nil {
		return fmt.Errorf("determine session type: %w", err)
	}
	if interactive {
		return errors.New("this command is started by Windows. To run ALL SHARE yourself, use: allshare-agent run")
	}

	writer, err := eventlog.Open(ServiceName)
	if err == nil {
		defer writer.Close()
	}
	logger := newServiceLogger(writer)
	return svc.Run(ServiceName, &allShareService{log: logger})
}

// Execute is the Windows service control loop.
func (s *allShareService) Execute(args []string, requests <-chan svc.ChangeRequest,
	status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {

	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	supervisor := newHostSupervisor(s.log)
	go supervisor.Run(ctx)

	status <- svc.Status{State: svc.Running, Accepts: accepted}
	s.log.Info("ALL SHARE service started")

	for request := range requests {
		switch request.Cmd {
		case svc.Interrogate:
			status <- request.CurrentStatus

		case svc.Stop, svc.Shutdown:
			s.log.Info("ALL SHARE service stopping")
			status <- svc.Status{State: svc.StopPending}
			cancel()
			supervisor.Wait(5 * time.Second)
			return false, 0

		case svc.SessionChange:
			// A logon, logoff, lock, unlock or fast user switch all mean the
			// interactive desktop has changed. The helper is running on the old
			// one and can no longer capture or inject, so it is replaced.
			s.log.Info("interactive session changed", "event", sessionChangeName(request.EventType))
			supervisor.Restart("the Windows session changed")
			status <- request.CurrentStatus

		default:
			status <- request.CurrentStatus
		}
	}
	return false, 0
}

func sessionChangeName(event uint32) string {
	switch event {
	case windows.WTS_CONSOLE_CONNECT:
		return "console connected"
	case windows.WTS_CONSOLE_DISCONNECT:
		return "console disconnected"
	case windows.WTS_SESSION_LOGON:
		return "user signed in"
	case windows.WTS_SESSION_LOGOFF:
		return "user signed out"
	case windows.WTS_SESSION_LOCK:
		return "workstation locked"
	case windows.WTS_SESSION_UNLOCK:
		return "workstation unlocked"
	default:
		return fmt.Sprintf("event %d", event)
	}
}

// ---------------------------------------------------------------------------
// Host supervision
// ---------------------------------------------------------------------------

// hostSupervisor keeps one agent process alive in the interactive session.
type hostSupervisor struct {
	log *slog.Logger

	mu      sync.Mutex
	process *os.Process
	restart chan string
	done    chan struct{}
	once    sync.Once
}

func newHostSupervisor(log *slog.Logger) *hostSupervisor {
	return &hostSupervisor{
		log:     log,
		restart: make(chan string, 4),
		done:    make(chan struct{}),
	}
}

// Restart asks for the helper to be replaced.
func (h *hostSupervisor) Restart(reason string) {
	select {
	case h.restart <- reason:
	default:
	}
}

// Wait blocks until the supervisor has stopped, up to a timeout.
func (h *hostSupervisor) Wait(timeout time.Duration) {
	select {
	case <-h.done:
	case <-time.After(timeout):
	}
}

func (h *hostSupervisor) Run(ctx context.Context) {
	defer h.once.Do(func() { close(h.done) })

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			h.kill()
			return
		default:
		}

		started := time.Now()
		err := h.launchAndWait(ctx)
		h.kill()

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Warn("the ALL SHARE helper stopped", "err", err)
		}

		// A helper that ran for a while hit something transient — a desktop
		// switch, a driver reset. One that died immediately is a real problem,
		// so the wait grows rather than spinning.
		if time.Since(started) > 60*time.Second {
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		case reason := <-h.restart:
			h.log.Info("restarting the ALL SHARE helper", "reason", reason)
		}
	}
}

// launchAndWait starts the helper in the interactive session and waits for it.
func (h *hostSupervisor) launchAndWait(ctx context.Context) error {
	process, err := launchInActiveSession()
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.process = process
	h.mu.Unlock()
	h.log.Info("ALL SHARE helper started", "pid", process.Pid)

	exited := make(chan error, 1)
	go func() {
		_, waitErr := process.Wait()
		exited <- waitErr
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case reason := <-h.restart:
		h.log.Info("replacing the ALL SHARE helper", "reason", reason)
		return nil
	case err := <-exited:
		return err
	}
}

func (h *hostSupervisor) kill() {
	h.mu.Lock()
	process := h.process
	h.process = nil
	h.mu.Unlock()
	if process == nil {
		return
	}
	_ = process.Kill()
	// Give Windows a moment to release the capture device before the next
	// helper tries to take it; Desktop Duplication allows a limited number of
	// simultaneous duplications per output.
	time.Sleep(300 * time.Millisecond)
}

// launchInActiveSession starts the agent inside the interactive Windows session.
//
// This is the documented way across the session-0 boundary: ask Terminal
// Services which session owns the console, take that user's token, and create
// the process with it on the interactive window station and desktop.
func launchInActiveSession() (*os.Process, error) {
	sessionID, _, _ := procWTSGetActiveConsoleID.Call()
	if uint32(sessionID) == 0xFFFFFFFF {
		return nil, errors.New("no interactive session is available yet")
	}

	var userToken windows.Token
	ret, _, err := procWTSQueryUserToken.Call(sessionID, uintptr(unsafe.Pointer(&userToken)))
	if ret == 0 {
		// Normal at the sign-in screen before anyone has signed in. The service
		// keeps retrying; there is genuinely nothing to capture until a session
		// exists.
		return nil, fmt.Errorf("nobody is signed in to this PC yet: %w", err)
	}
	defer userToken.Close()

	var primary windows.Token
	if err := windows.DuplicateTokenEx(userToken, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return nil, fmt.Errorf("duplicate the signed-in user's token: %w", err)
	}
	defer primary.Close()

	// The signed-in user's environment, so the helper sees the same PATH,
	// TEMP and locale the user would.
	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, primary, false); err != nil {
		return nil, fmt.Errorf("build the user's environment: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(environment)

	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(
		fmt.Sprintf(`"%s" run --supervised`, executable))
	if err != nil {
		return nil, err
	}
	// The interactive window station and its default desktop. This is what
	// makes screen capture and input injection possible at all.
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return nil, err
	}
	directory, err := windows.UTF16PtrFromString(filepath.Dir(executable))
	if err != nil {
		return nil, err
	}

	startup := windows.StartupInfo{
		Cb:      uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop: desktop,
	}
	var info windows.ProcessInformation

	err = windows.CreateProcessAsUser(primary, nil, commandLine, nil, nil, false,
		createUnicodeEnvironment|createNoWindow,
		environment, directory, &startup, &info)
	if err != nil {
		return nil, fmt.Errorf("start ALL SHARE in the signed-in session: %w", err)
	}
	windows.CloseHandle(info.Thread)

	process, err := os.FindProcess(int(info.ProcessId))
	if err != nil {
		windows.CloseHandle(info.Process)
		return nil, err
	}
	windows.CloseHandle(info.Process)
	return process, nil
}

// cmdHost runs the desktop-side worker. It is an alias for "run" so that the
// service's command line reads clearly in Task Manager.
func cmdHost(args []string) error {
	return cmdRun(append([]string{"--supervised"}, args...))
}

// ---------------------------------------------------------------------------
// Wake check-in
// ---------------------------------------------------------------------------

// cmdCheckIn is what the scheduled wake timer runs.
//
// The PC wakes, this asks the rendezvous whether anyone is waiting, and if
// nobody is it exits so the machine can go straight back to sleep. If someone
// is waiting, the service's helper is already connecting and this simply holds
// the machine awake long enough for that to finish.
func cmdCheckIn(args []string) error {
	flags := flag.NewFlagSet("checkin", flag.ExitOnError)
	dataDir := flags.String("data", "", "directory for settings and identity")
	hold := flags.Duration("hold", 90*time.Second, "how long to stay awake while checking")
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
	if cfg.Rendezvous == "" {
		return nil
	}

	// The service is already running and will connect on its own. All this has
	// to do is keep the machine out of sleep long enough for that to happen and
	// for a waiting client to be told the PC is up.
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	setExecutionState := kernel32.NewProc("SetThreadExecutionState")
	setExecutionState.Call(uintptr(0x80000000 | 0x00000001)) // continuous | system required
	defer setExecutionState.Call(uintptr(0x80000000))

	time.Sleep(*hold)
	return nil
}

// ---------------------------------------------------------------------------
// Install and uninstall
// ---------------------------------------------------------------------------

func cmdInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	service := flags.String("service", "", "ALL SHARE service address")
	name := flags.String("name", "", "name to show for this PC")
	if err := flags.Parse(args); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("this needs to run as an administrator: %w", err)
	}
	defer manager.Disconnect()

	if existing, err := manager.OpenService(ServiceName); err == nil {
		existing.Close()
		return fmt.Errorf("ALL SHARE is already installed. Run \"allshare-agent uninstall\" first")
	}

	installed, err := manager.CreateService(ServiceName, executable, mgr.Config{
		DisplayName:  ServiceDisplayName,
		Description:  serviceDescription,
		StartType:    mgr.StartAutomatic,
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		ErrorControl: mgr.ErrorNormal,
		// Delayed start keeps ALL SHARE from competing with the rest of the
		// boot sequence; a couple of seconds later costs the user nothing.
		DelayedAutoStart: true,
	}, "service")
	if err != nil {
		return fmt.Errorf("create the ALL SHARE service: %w", err)
	}
	defer installed.Close()

	// Restart on failure, forever. A remote-access agent that gives up after
	// three crashes is a remote-access agent that is not there when needed.
	if err := installed.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		// Not fatal; the service still works, it just will not self-heal.
		fmt.Fprintln(os.Stderr, "note: could not set automatic restart:", err)
	}

	if err := eventlog.InstallAsEventCreate(ServiceName,
		eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		fmt.Fprintln(os.Stderr, "note: could not register event logging:", err)
	}

	// Settings are written before the service starts so its first connection
	// attempt already has somewhere to go.
	if *service != "" || *name != "" {
		dir, err := config.Dir()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "config.json")
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.SetPath(path)
		if err := cfg.Update(func(c *config.Config) {
			if *service != "" {
				c.Rendezvous = *service
			}
			if *name != "" {
				c.DeviceName = *name
			}
		}); err != nil {
			return err
		}
	}

	if err := installed.Start(); err != nil {
		return fmt.Errorf("the service was installed but would not start: %w", err)
	}

	fmt.Println("ALL SHARE is installed and running.")
	fmt.Println("It will start automatically whenever this PC starts.")
	fmt.Println()
	fmt.Println("Next: run \"allshare-agent pair\" to add a device.")
	return nil
}

func cmdUninstall(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	keepSettings := flags.Bool("keep-settings", false, "leave settings and paired devices in place")
	if err := flags.Parse(args); err != nil {
		return err
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("this needs to run as an administrator: %w", err)
	}
	defer manager.Disconnect()

	installed, err := manager.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("ALL SHARE does not appear to be installed: %w", err)
	}
	defer installed.Close()

	if status, err := installed.Control(svc.Stop); err == nil {
		deadline := time.Now().Add(20 * time.Second)
		for status.State != svc.Stopped && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			status, err = installed.Query()
			if err != nil {
				break
			}
		}
	}

	if err := installed.Delete(); err != nil {
		return fmt.Errorf("remove the ALL SHARE service: %w", err)
	}
	_ = eventlog.Remove(ServiceName)

	// The scheduled wake timer is removed too, or the PC would keep waking for
	// a program that is no longer there.
	cmd := exec.Command("schtasks", "/Delete", "/TN", "ALL SHARE Wake Check-In", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()

	if !*keepSettings {
		if dir, err := config.Dir(); err == nil {
			// The identity is deliberately kept unless settings are cleared:
			// losing it un-pairs every device the user has set up.
			_ = os.Remove(filepath.Join(dir, "config.json"))
		}
	}

	fmt.Println("ALL SHARE has been removed from this PC.")
	return nil
}

// newServiceLogger sends log records to the Windows event log as well as stderr.
func newServiceLogger(writer *eventlog.Log) *slog.Logger {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	if writer == nil {
		return slog.New(handler)
	}
	return slog.New(&eventLogHandler{inner: handler, writer: writer})
}

type eventLogHandler struct {
	inner  slog.Handler
	writer *eventlog.Log
}

func (h *eventLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *eventLogHandler) Handle(ctx context.Context, record slog.Record) error {
	var builder strings.Builder
	builder.WriteString(record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		builder.WriteString(" ")
		builder.WriteString(attr.Key)
		builder.WriteString("=")
		fmt.Fprintf(&builder, "%v", attr.Value.Any())
		return true
	})
	text := builder.String()

	switch {
	case record.Level >= slog.LevelError:
		_ = h.writer.Error(1, text)
	case record.Level >= slog.LevelWarn:
		_ = h.writer.Warning(1, text)
	default:
		_ = h.writer.Info(1, text)
	}
	return h.inner.Handle(ctx, record)
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventLogHandler{inner: h.inner.WithAttrs(attrs), writer: h.writer}
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	return &eventLogHandler{inner: h.inner.WithGroup(name), writer: h.writer}
}
