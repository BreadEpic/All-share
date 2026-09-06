// Package wake implements ALL SHARE's wake-the-PC machinery.
//
// The honest position, which the rest of the product is built around: a
// Wake-on-LAN magic packet is a layer-2 broadcast and cannot be delivered to a
// sleeping machine from the internet. A home router has no ARP entry for a
// powered-down host, and essentially no consumer router will forward a packet
// to a subnet broadcast address. Any design that claims otherwise works on a
// LAN and fails silently everywhere else.
//
// So this package reports what a given PC can genuinely support, and the client
// shows that rather than a button that might do nothing:
//
//	modernStandby — the machine keeps its network connection through sleep, so
//	                the agent is still reachable and nothing needs waking.
//	lanPeer       — another ALL SHARE PC on the same network sends the packet.
//	checkin       — this PC arms a wake timer and wakes briefly on a schedule to
//	                ask whether anyone wants it. Needs no second machine and no
//	                router configuration, at the cost of waiting.
//	none          — with the specific reason, in plain language.
package wake

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mmc/all-share/shared/protocol"
)

// Sender emits Wake-on-LAN magic packets on the local network.
type Sender struct{}

// MagicPacket builds the 102-byte Wake-on-LAN payload for a MAC address: six
// 0xFF bytes followed by the address repeated sixteen times.
func MagicPacket(mac string) ([]byte, error) {
	hardware, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("allshare/wake: %q is not a MAC address: %w", mac, err)
	}
	if len(hardware) != 6 {
		return nil, fmt.Errorf("allshare/wake: wake-on-LAN needs a 6-byte MAC address, got %d bytes", len(hardware))
	}
	packet := make([]byte, 0, 6+16*6)
	for i := 0; i < 6; i++ {
		packet = append(packet, 0xFF)
	}
	for i := 0; i < 16; i++ {
		packet = append(packet, hardware...)
	}
	return packet, nil
}

// Send broadcasts magic packets for each MAC to each broadcast address.
//
// Both port 9 and port 7 are used because NICs differ in which they listen on,
// and a magic packet is small enough that sending both costs nothing.
func (Sender) Send(macs, broadcasts []string) error {
	if len(macs) == 0 {
		return fmt.Errorf("allshare/wake: no MAC address to wake")
	}
	if len(broadcasts) == 0 {
		broadcasts = LocalBroadcastAddresses()
	}
	if len(broadcasts) == 0 {
		broadcasts = []string{"255.255.255.255"}
	}

	var firstErr error
	sent := 0
	for _, mac := range macs {
		packet, err := MagicPacket(mac)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, broadcast := range broadcasts {
			for _, port := range []string{"9", "7"} {
				conn, err := net.DialTimeout("udp", net.JoinHostPort(broadcast, port), 2*time.Second)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				_, err = conn.Write(packet)
				_ = conn.Close()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				sent++
			}
		}
	}
	if sent == 0 && firstErr != nil {
		return firstErr
	}
	return nil
}

// LocalBroadcastAddresses lists the directed broadcast address of every
// IPv4 network this machine is on.
//
// Directed broadcast is used in preference to 255.255.255.255 because a host
// with several interfaces would otherwise send only on whichever one the
// routing table picks, which may not be the one the sleeping PC is on.
func LocalBroadcastAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4()
			mask := ipnet.Mask
			if len(mask) != 4 {
				continue
			}
			broadcast := net.IPv4(
				ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3]).String()
			if !seen[broadcast] {
				seen[broadcast] = true
				out = append(out, broadcast)
			}
		}
	}
	return out
}

// LocalMACAddresses lists the MAC addresses of this machine's active,
// non-virtual network interfaces.
func LocalMACAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) != 6 {
			continue
		}
		// A machine can have a dozen virtual adapters; none of them can be
		// woken, and listing them would only add noise.
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		out = append(out, iface.HardwareAddr.String())
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func isVirtualInterface(name string) bool {
	for _, prefix := range []string{
		"vEthernet", "VMware", "VirtualBox", "Hyper-V", "Loopback",
		"docker", "veth", "br-", "tun", "tap", "utun", "wg",
	} {
		if len(name) >= len(prefix) && equalFold(name[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Manager reports and exercises this machine's wake capability.
type Manager struct {
	mu sync.Mutex

	// Platform supplies the OS-specific facts. On anything but Windows it
	// reports that scheduled wake is unavailable, which is the truth.
	platform Platform

	checkInMinutes int
	enabled        bool
	sender         Sender

	holdUntil time.Time
	holdStop  chan struct{}
}

// Platform is the operating-system-specific half of wake support.
type Platform interface {
	// SupportsModernStandby reports whether this machine stays network-connected
	// while asleep, which makes waking unnecessary.
	SupportsModernStandby() bool
	// WakeArmed reports whether a network adapter is configured to wake the
	// machine, and why not if it is not.
	WakeArmed() (bool, string)
	// ScheduleCheckIn arms a wake timer to fire every interval. Passing zero
	// cancels it.
	ScheduleCheckIn(interval time.Duration) error
	// PreventSleep holds off sleep, returning a function that releases the hold.
	PreventSleep(reason string) (release func(), err error)
}

// NewManager builds a wake manager.
func NewManager(platform Platform, checkInMinutes int, enabled bool) *Manager {
	return &Manager{platform: platform, checkInMinutes: checkInMinutes, enabled: enabled}
}

// Capability describes how, if at all, this PC can be woken.
func (m *Manager) Capability() protocol.WakeCapability {
	m.mu.Lock()
	enabled := m.enabled
	checkIn := m.checkInMinutes
	m.mu.Unlock()

	capability := protocol.WakeCapability{
		MACAddresses:   LocalMACAddresses(),
		BroadcastAddrs: LocalBroadcastAddresses(),
	}

	if !enabled {
		capability.Method = "none"
		capability.Reason = "Waking this PC remotely is turned off in ALL SHARE's settings."
		return capability
	}

	if m.platform == nil {
		capability.Method = "none"
		capability.Reason = "This build of ALL SHARE cannot wake this computer."
		return capability
	}

	armed, why := m.platform.WakeArmed()
	capability.Armed = armed

	// Modern standby is the best case: the machine never really leaves the
	// network, so there is nothing to wake and the connection simply resumes.
	if m.platform.SupportsModernStandby() {
		capability.Method = "modernStandby"
		capability.EstimatedSeconds = 5
		return capability
	}

	// A magic packet from a neighbour is the fastest of the remaining options,
	// but only if the NIC is actually configured to listen for one.
	if armed && len(capability.MACAddresses) > 0 {
		capability.Method = "lanPeer"
		capability.EstimatedSeconds = 20
		// Check-in is the fallback when no neighbour is online. The server
		// picks whichever it can actually deliver.
		if checkIn > 0 {
			capability.EstimatedSeconds = 20
		}
		return capability
	}

	if checkIn > 0 {
		capability.Method = "checkin"
		capability.EstimatedSeconds = checkIn * 60
		if !armed && why != "" {
			capability.Reason = why
		}
		return capability
	}

	capability.Method = "none"
	if why != "" {
		capability.Reason = why
	} else {
		capability.Reason = "This PC is not set up to be woken remotely. Turn on scheduled check-ins in ALL SHARE's settings."
	}
	return capability
}

// Apply arms or cancels the scheduled check-in to match the settings.
func (m *Manager) Apply() error {
	m.mu.Lock()
	enabled := m.enabled
	checkIn := m.checkInMinutes
	m.mu.Unlock()

	if m.platform == nil {
		return nil
	}
	if !enabled || checkIn <= 0 || m.platform.SupportsModernStandby() {
		return m.platform.ScheduleCheckIn(0)
	}
	return m.platform.ScheduleCheckIn(time.Duration(checkIn) * time.Minute)
}

// Configure updates the wake settings and re-applies them.
func (m *Manager) Configure(checkInMinutes int, enabled bool) error {
	m.mu.Lock()
	m.checkInMinutes = checkInMinutes
	m.enabled = enabled
	m.mu.Unlock()
	return m.Apply()
}

// WakePeer sends a magic packet on behalf of a neighbour.
func (m *Manager) WakePeer(macs, broadcasts []string) error {
	return m.sender.Send(macs, broadcasts)
}

// StayAwake holds off sleep for a while.
//
// Called after this PC wakes on someone's behalf, so the machine does not drop
// straight back to sleep before the user has finished connecting.
func (m *Manager) StayAwake(d time.Duration, reason string) {
	if m.platform == nil || d <= 0 {
		return
	}
	m.mu.Lock()
	until := time.Now().Add(d)
	if until.After(m.holdUntil) {
		m.holdUntil = until
	}
	if m.holdStop != nil {
		m.mu.Unlock()
		return // an existing hold will extend to the new deadline
	}
	stop := make(chan struct{})
	m.holdStop = stop
	m.mu.Unlock()

	release, err := m.platform.PreventSleep(reason)
	if err != nil {
		m.mu.Lock()
		m.holdStop = nil
		m.mu.Unlock()
		return
	}

	go func() {
		defer release()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				expired := time.Now().After(m.holdUntil)
				if expired {
					m.holdStop = nil
				}
				m.mu.Unlock()
				if expired {
					return
				}
			}
		}
	}()
}
