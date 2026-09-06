package signal

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/mmc/all-share/server/internal/registry"
	"github.com/mmc/all-share/shared/protocol"
)

// Waking a sleeping PC over the internet is the part of remote access that most
// often gets hand-waved, so this file is explicit about what is actually
// possible.
//
// A Wake-on-LAN magic packet is a layer-2 broadcast. It cannot be routed to a
// sleeping machine from the internet: the home router has no ARP entry for a
// powered-down host, and virtually no consumer router will forward a packet to
// a subnet broadcast address. Sending a magic packet "from the Chromebook" is
// therefore not a design — it is a thing that appears to work on a LAN and
// silently fails everywhere else.
//
// ALL SHARE instead picks the best of four honest strategies, in this order:
//
//  1. modernStandby — on a machine using S0 low-power idle the agent keeps its
//     rendezvous connection alive through sleep. Nothing needs waking; the PC
//     is already reachable and simply resumes. Instant.
//
//  2. lanPeer — another ALL SHARE agent on the same local network sends the
//     magic packet for us. This is the reliable path and needs no router
//     configuration at all. A few seconds.
//
//  3. checkin — the PC arms a Windows wake timer and briefly wakes on a
//     schedule to ask whether anyone wants it. Needs no second machine and no
//     network configuration whatsoever, at the cost of waiting up to one
//     check-in interval. The UI shows that interval rather than pretending the
//     wake is instant.
//
//  4. wolDirect — the rendezvous server is itself on the same LAN (a
//     self-hosted deployment) and can broadcast directly.
//
// When none of these apply the client is told so plainly, with the specific
// reason, instead of being shown a button that does nothing.

// wakeHoldSeconds is how long a freshly woken agent is asked to stay awake.
// Long enough for a user to finish connecting, short enough that a stray wake
// does not keep a machine up all night.
const wakeHoldSeconds = 180

// wakeRequestTTL bounds how long a pending check-in wake stays armed.
const wakeRequestTTL = 30 * time.Minute

type pendingWake struct {
	clientID  string
	requested time.Time
	expires   time.Time
}

// wakeState tracks check-in wakes awaiting their machine.
type wakeState struct {
	mu      sync.Mutex
	pending map[string]pendingWake
}

func newWakeState() *wakeState { return &wakeState{pending: map[string]pendingWake{}} }

func (w *wakeState) arm(deviceID, clientID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for id, p := range w.pending {
		if now.After(p.expires) {
			delete(w.pending, id)
		}
	}
	w.pending[deviceID] = pendingWake{clientID: clientID, requested: now, expires: now.Add(wakeRequestTTL)}
}

// take returns and clears a pending wake for a device.
func (w *wakeState) take(deviceID string) (pendingWake, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.pending[deviceID]
	delete(w.pending, deviceID)
	if !ok || time.Now().After(p.expires) {
		return pendingWake{}, false
	}
	return p, true
}

func (h *Hub) onWake(c *Conn, env protocol.Envelope) error {
	if c.role != protocol.RoleClient {
		return h.reject(c, env, protocol.ErrUnauthorized, "Only a client can wake a PC.")
	}
	var req protocol.WakeBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		return h.reject(c, env, protocol.ErrBadRequest, "That wake request was not understood.")
	}
	if !h.connectLimit.Allow("wake:" + c.deviceID) {
		return h.reject(c, env, protocol.ErrRateLimited, "Please wait a few seconds before trying to wake this PC again.")
	}
	dev, err := h.reg.Get(req.Target)
	if err != nil {
		return h.reject(c, env, protocol.ErrDeviceUnknown, "That PC is not set up with ALL SHARE any more.")
	}
	if !dev.IsPairedWith(c.deviceID) {
		return h.reject(c, env, protocol.ErrNotPaired, "This device is not paired with that PC.")
	}

	status := h.performWake(dev, c.deviceID)
	c.writeReply(protocol.MsgWakeStatus, env.Ref, status)
	c.log.Info("wake requested", "target", shortID(req.Target), "method", status.Method, "stage", status.Stage)
	return nil
}

// performWake chooses and executes the best available strategy.
func (h *Hub) performWake(dev *registry.Device, clientID string) protocol.WakeStatus {
	status := protocol.WakeStatus{Target: dev.ID}

	if h.agentConn(dev.ID) != nil {
		status.Stage = "online"
		status.Method = "already-online"
		status.Message = "This PC is already awake."
		return status
	}

	// 2. A neighbour on the same local network can deliver a magic packet.
	if dev.Wake.LANKey != "" && len(dev.Wake.MACAddresses) > 0 {
		for _, peer := range h.reg.PeersOnLAN(dev.Wake.LANKey, dev.ID) {
			helper := h.agentConn(peer.ID)
			if helper == nil {
				continue
			}
			helper.writeMsg(protocol.MsgWakePeer, protocol.WakePeerBody{
				Target:         dev.ID,
				MACAddresses:   dev.Wake.MACAddresses,
				BroadcastAddrs: dev.Wake.BroadcastAddrs,
			})
			h.wake.arm(dev.ID, clientID)
			status.Stage = "sent"
			status.Method = "lanPeer"
			status.EstimatedSeconds = 20
			status.Message = "Waking your PC. This usually takes a few seconds."
			return status
		}
	}

	// 4. A self-hosted server on the same network can broadcast itself.
	if h.wolSender != nil && len(dev.Wake.MACAddresses) > 0 && h.wolSender.SameNetwork(dev.Wake.LANKey) {
		if err := h.wolSender.Send(dev.Wake.MACAddresses, dev.Wake.BroadcastAddrs); err == nil {
			h.wake.arm(dev.ID, clientID)
			status.Stage = "sent"
			status.Method = "wolDirect"
			status.EstimatedSeconds = 20
			status.Message = "Waking your PC. This usually takes a few seconds."
			return status
		}
	}

	// 3. The PC wakes itself on a schedule and asks whether it is wanted.
	if dev.Wake.Method == "checkin" && dev.Wake.EstimatedSeconds > 0 {
		h.wake.arm(dev.ID, clientID)
		status.Stage = "waiting"
		status.Method = "checkin"
		status.EstimatedSeconds = dev.Wake.EstimatedSeconds
		status.Message = "Your PC checks in periodically. It will come online shortly and connect automatically."
		return status
	}

	status.Stage = "unavailable"
	status.Method = dev.Wake.Method
	status.Code = protocol.ErrWakeUnavailable
	status.Message = wakeUnavailableMessage(dev)
	return status
}

// wakeUnavailableMessage explains, in the user's terms, why no wake is possible
// and what they can change. A generic failure here would leave the user with a
// dead button and no idea why.
func wakeUnavailableMessage(dev *registry.Device) string {
	if dev.Wake.Reason != "" {
		return dev.Wake.Reason
	}
	if !dev.Wake.Armed {
		return "This PC cannot be woken remotely yet. Open ALL SHARE on the PC and turn on \"Let me wake this PC remotely\"."
	}
	return "This PC cannot be woken from here. Waking needs either another ALL SHARE PC on the same home network, or scheduled check-ins turned on in ALL SHARE on the PC."
}

// noteAgentOnline is called when an agent connects. If someone was waiting for
// this machine, it holds off sleep and the waiting client is told immediately.
func (h *Hub) noteAgentOnline(deviceID string) {
	p, ok := h.wake.take(deviceID)
	if !ok {
		return
	}
	if agent := h.agentConn(deviceID); agent != nil {
		agent.writeMsg(protocol.MsgStayAwake, protocol.StayAwakeBody{
			Seconds: wakeHoldSeconds,
			Reason:  "A paired device asked for this PC to be woken.",
		})
	}
	h.sendToClient(p.clientID, protocol.MsgWakeStatus, "", protocol.WakeStatus{
		Target:  deviceID,
		Stage:   "online",
		Method:  "wake",
		Message: "Your PC is awake.",
	})
	h.log.Info("woken PC came online", "device", shortID(deviceID), "after", time.Since(p.requested).Round(time.Second))
}

// WOLSender broadcasts magic packets from the server's own network.
type WOLSender interface {
	// SameNetwork reports whether the server shares a LAN identity with a device.
	SameNetwork(lanKey string) bool
	// Send emits magic packets for the given MACs.
	Send(macs, broadcasts []string) error
}

// LocalWOL is the built-in sender used when the rendezvous is self-hosted on
// the same network as the PCs it serves.
type LocalWOL struct {
	// LANKey is this server's own LAN identity, computed the same way an
	// agent's is.
	LANKey string
}

// SameNetwork reports whether a device shares this server's local network.
func (l *LocalWOL) SameNetwork(lanKey string) bool {
	return l.LANKey != "" && lanKey == l.LANKey
}

// Send broadcasts a magic packet for each MAC to each broadcast address.
func (l *LocalWOL) Send(macs, broadcasts []string) error {
	if len(broadcasts) == 0 {
		broadcasts = []string{"255.255.255.255"}
	}
	var firstErr error
	for _, mac := range macs {
		packet, err := MagicPacket(mac)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, bcast := range broadcasts {
			for _, port := range []string{"9", "7"} {
				addr := net.JoinHostPort(bcast, port)
				conn, err := net.Dial("udp", addr)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				_, err = conn.Write(packet)
				conn.Close()
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// MagicPacket builds the 102-byte Wake-on-LAN payload for a MAC address:
// six 0xFF bytes followed by the address repeated sixteen times.
func MagicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	if len(hw) != 6 {
		return nil, &net.AddrError{Err: "wake-on-lan needs a 6-byte MAC address", Addr: mac}
	}
	packet := make([]byte, 0, 6+16*6)
	for i := 0; i < 6; i++ {
		packet = append(packet, 0xFF)
	}
	for i := 0; i < 16; i++ {
		packet = append(packet, hw...)
	}
	return packet, nil
}

// SetWOL installs a direct magic-packet sender, used when the rendezvous is
// self-hosted on the same network as the PCs it serves.
func (h *Hub) SetWOL(s WOLSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wolSender = s
}
