//go:build windows

package wake

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecution = kernel32.NewProc("SetThreadExecutionState")

	powrprof               = windows.NewLazySystemDLL("powrprof.dll")
	procGetPwrCapabilities = powrprof.NewProc("GetPwrCapabilities")
)

const (
	esSystemRequired = 0x00000001
	esAwaymodeReq    = 0x00000040
	esContinuous     = 0x80000000
)

// systemPowerCapabilities mirrors the Win32 SYSTEM_POWER_CAPABILITIES struct.
//
// Only the fields ALL SHARE reads are named; the rest is padding to the correct
// size, because the struct is passed to Windows by size and a short one would
// be a buffer overrun.
type systemPowerCapabilities struct {
	PowerButtonPresent        byte
	SleepButtonPresent        byte
	LidPresent                byte
	SystemS1                  byte
	SystemS2                  byte
	SystemS3                  byte
	SystemS4                  byte
	SystemS5                  byte
	HiberFilePresent          byte
	FullWake                  byte
	VideoDimPresent           byte
	ApmPresent                byte
	UpsPresent                byte
	ThermalControl            byte
	ProcessorThrottle         byte
	ProcessorMinThrottle      byte
	ProcessorMaxThrottle      byte
	FastSystemS4              byte
	Hiberboot                 byte
	WakeAlarmPresent          byte
	AoAc                      byte // "always on, always connected" — modern standby
	DiskSpinDown              byte
	HiberFileType             byte
	AoAcConnectivitySupported byte
	spare3                    [6]byte
	SystemBatteriesPresent    byte
	BatteriesAreShortTerm     byte
	BatteryScale              [3]struct {
		Granularity uint32
		Capacity    uint32
	}
	AcOnLineWake          int32
	SoftLidWake           int32
	RtcWake               int32
	MinDeviceWakeState    int32
	DefaultLowLatencyWake int32
}

// windowsPlatform implements wake support on Windows.
type windowsPlatform struct {
	mu        sync.Mutex
	taskName  string
	agentPath string
}

// NewPlatform returns the wake platform for this operating system.
func NewPlatform() Platform {
	exe, err := os.Executable()
	if err != nil {
		exe = "allshare-agent.exe"
	}
	return &windowsPlatform{taskName: `ALL SHARE Wake Check-In`, agentPath: exe}
}

// SupportsModernStandby reports whether this machine keeps its network
// connection through sleep.
//
// On such a machine there is nothing to wake: the agent stays reachable and the
// PC simply resumes. That is the best possible answer, so it is checked first.
func (p *windowsPlatform) SupportsModernStandby() bool {
	if err := procGetPwrCapabilities.Find(); err != nil {
		return false
	}
	var caps systemPowerCapabilities
	ret, _, _ := procGetPwrCapabilities.Call(uintptr(unsafeSlicePointer(&caps)))
	if ret == 0 {
		return false
	}
	// AoAc is set on machines using S0 low-power idle. Those never enter S3, so
	// a wake timer would never fire and a magic packet is unnecessary.
	return caps.AoAc != 0
}

// WakeArmed reports whether a network adapter is configured to wake this PC.
func (p *windowsPlatform) WakeArmed() (bool, string) {
	// powercfg reports exactly what Windows itself believes is armed, which is
	// more trustworthy than inspecting driver registry keys by hand.
	out, err := runHidden("powercfg", "/devicequery", "wake_armed")
	if err != nil {
		return false, "ALL SHARE could not check this PC's wake settings."
	}
	text := strings.TrimSpace(out)
	if text == "" || strings.EqualFold(text, "NONE") {
		return false, "No network adapter on this PC is set to wake it. " +
			"In Device Manager, open your network adapter's properties and turn on " +
			"\"Allow this device to wake the computer\"."
	}
	// Any armed device is worth reporting, but only a network adapter can be
	// woken remotely, so the message stays specific.
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "ethernet") || strings.Contains(lower, "wi-fi") ||
			strings.Contains(lower, "wireless") || strings.Contains(lower, "network") ||
			strings.Contains(lower, "nic") || strings.Contains(lower, "lan") {
			return true, ""
		}
	}
	return false, "This PC can be woken, but not by the network. " +
		"In Device Manager, open your network adapter's properties and turn on " +
		"\"Allow this device to wake the computer\"."
}

// ScheduleCheckIn arms a wake timer that briefly wakes this PC on a schedule.
//
// This is the path that needs nothing from the network at all: no second
// machine, no router configuration, no port forwarding. The PC wakes, the agent
// connects, the rendezvous tells it whether anyone is waiting, and if nobody is
// it goes straight back to sleep. The cost is the wait, which the client shows
// honestly rather than implying an instant wake.
func (p *windowsPlatform) ScheduleCheckIn(interval time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if interval <= 0 {
		_, _ = runHidden("schtasks", "/Delete", "/TN", p.taskName, "/F")
		return nil
	}
	minutes := int(interval.Minutes())
	if minutes < 5 {
		// Below a few minutes the machine spends more time waking than asleep,
		// and the battery cost stops being reasonable.
		minutes = 5
	}
	if minutes > 720 {
		minutes = 720
	}

	// Delete first: schtasks has no "create or replace" that also updates the
	// wake flag reliably across Windows versions.
	_, _ = runHidden("schtasks", "/Delete", "/TN", p.taskName, "/F")

	args := []string{
		"/Create", "/TN", p.taskName,
		"/TR", fmt.Sprintf(`"%s" checkin`, p.agentPath),
		"/SC", "MINUTE", "/MO", fmt.Sprint(minutes),
		"/RU", "SYSTEM", "/RL", "HIGHEST", "/F",
	}
	if _, err := runHidden("schtasks", args...); err != nil {
		return fmt.Errorf("allshare/wake: could not create the wake schedule: %w", err)
	}

	// schtasks cannot set the wake flag, so it is applied with PowerShell's
	// scheduled-task settings, which can.
	//
	// The task name is a compile-time constant and reaches nothing from the
	// network, so this is not an injection today. It is quoted properly anyway,
	// because the day someone makes the name configurable is the day an
	// unescaped interpolation becomes one, and that change would not look
	// dangerous on its own.
	name := psQuote(p.taskName)
	script := fmt.Sprintf(
		`$t = Get-ScheduledTask -TaskName %s; `+
			`$s = $t.Settings; $s.WakeToRun = $true; $s.DisallowStartIfOnBatteries = $false; `+
			`$s.StopIfGoingOnBatteries = $false; $s.ExecutionTimeLimit = 'PT5M'; `+
			`Set-ScheduledTask -TaskName %s -Settings $s | Out-Null`,
		name, name)
	if _, err := runHidden("powershell", "-NoProfile", "-NonInteractive", "-Command", script); err != nil {
		return fmt.Errorf("allshare/wake: the schedule was created but could not be set to wake this PC: %w", err)
	}
	return nil
}

// PreventSleep holds off sleep until the returned function is called.
func (p *windowsPlatform) PreventSleep(reason string) (func(), error) {
	if err := procSetThreadExecution.Find(); err != nil {
		return func() {}, err
	}
	// A hold must be renewed from the same thread that set it, so the flag is
	// held on a dedicated goroutine locked to its OS thread.
	stop := make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		lockOSThread()
		defer unlockOSThread()
		ret, _, err := procSetThreadExecution.Call(uintptr(esContinuous | esSystemRequired | esAwaymodeReq))
		if ret == 0 {
			// Away mode is not available everywhere; the plain hold is enough.
			ret, _, err = procSetThreadExecution.Call(uintptr(esContinuous | esSystemRequired))
		}
		if ret == 0 {
			ready <- err
			return
		}
		ready <- nil
		<-stop
		procSetThreadExecution.Call(uintptr(esContinuous))
	}()

	if err := <-ready; err != nil {
		return func() {}, fmt.Errorf("allshare/wake: could not hold off sleep: %w", err)
	}
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }, nil
}

// runHidden runs a command without flashing a console window, which matters
// when the agent runs from a service or a tray application.
// psQuote renders s as a PowerShell single-quoted string. Inside single quotes
// PowerShell expands nothing, so doubling the quote character is the whole of
// the escaping rule.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
