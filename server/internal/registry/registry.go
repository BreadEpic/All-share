// Package registry keeps the rendezvous server's view of known devices.
//
// The registry is deliberately thin. It stores which agents exist, what they
// can do, and which client keys each agent has said it trusts. It never stores
// secrets: no pairing codes, no session keys, no screen data. If the registry
// file leaks, an attacker learns a list of device names and public keys and
// gains no ability to connect to anything, because the agent independently
// enforces its own pairing list and every session is authenticated end to end.
//
// Storage is a single JSON document written atomically. A personal deployment
// has a handful of devices, so an embedded database would add a dependency and
// an operational surface for no measurable benefit; the store interface exists
// so a larger deployment can swap in something else.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mmc/all-share/shared/protocol"
)

// ErrNotFound reports an unknown device.
var ErrNotFound = errors.New("allshare/registry: device not found")

// MaxPairedClients bounds how many clients one agent may register, so a buggy
// or hostile agent cannot grow the registry without limit.
const MaxPairedClients = 64

// MaxDeviceNameLen bounds a device name. Names are shown in the client UI and
// echoed in logs, so they are length-limited and sanitised on the way in.
const MaxDeviceNameLen = 64

// Device is one registered agent machine.
type Device struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	OS           string                  `json:"os"`
	AgentVersion string                  `json:"agentVersion"`
	Wake         protocol.WakeCapability `json:"wake"`
	// PairedClients holds the client identity keys this agent says it trusts.
	// The server uses it only to decide what to list and what to route; the
	// agent re-checks every connection itself.
	PairedClients []string  `json:"pairedClients"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
}

// IsPairedWith reports whether a client key appears in the device's list.
func (d *Device) IsPairedWith(clientID string) bool {
	for _, c := range d.PairedClients {
		if c == clientID {
			return true
		}
	}
	return false
}

// Registry is a concurrency-safe device store with a durable JSON backing file.
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*Device
	path    string

	dirty    bool
	flushCh  chan struct{}
	closeCh  chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup
}

type persisted struct {
	Version int       `json:"version"`
	Devices []*Device `json:"devices"`
}

// Open loads the registry at path, creating an empty one if it does not exist.
//
// A background goroutine coalesces writes: presence updates arrive every few
// seconds per device and are not worth an fsync each.
func Open(path string) (*Registry, error) {
	r := &Registry{
		devices: map[string]*Device{},
		path:    path,
		flushCh: make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			return nil, err
		}
		r.wg.Add(1)
		go r.flushLoop()
	}
	return r, nil
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("allshare/registry: read %s: %w", r.path, err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("allshare/registry: parse %s: %w", r.path, err)
	}
	for _, d := range p.Devices {
		if d != nil && d.ID != "" {
			r.devices[d.ID] = d
		}
	}
	return nil
}

func (r *Registry) flushLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.closeCh:
			_ = r.Flush()
			return
		case <-r.flushCh:
			_ = r.Flush()
		case <-ticker.C:
			r.mu.RLock()
			dirty := r.dirty
			r.mu.RUnlock()
			if dirty {
				_ = r.Flush()
			}
		}
	}
}

func (r *Registry) markDirty() {
	r.dirty = true
	select {
	case r.flushCh <- struct{}{}:
	default:
	}
}

// Flush writes the registry to disk atomically.
func (r *Registry) Flush() error {
	if r.path == "" {
		return nil
	}
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	p := persisted{Version: 1, Devices: make([]*Device, 0, len(r.devices))}
	for _, d := range r.devices {
		copyOf := *d
		copyOf.PairedClients = append([]string(nil), d.PairedClients...)
		p.Devices = append(p.Devices, &copyOf)
	}
	r.dirty = false
	r.mu.Unlock()

	sort.Slice(p.Devices, func(i, j int) bool { return p.Devices[i].ID < p.Devices[j].ID })
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("allshare/registry: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("allshare/registry: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".registry-*")
	if err != nil {
		return fmt.Errorf("allshare/registry: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/registry: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/registry: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("allshare/registry: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("allshare/registry: chmod: %w", err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return fmt.Errorf("allshare/registry: install: %w", err)
	}
	return nil
}

// Close flushes and stops the background writer.
func (r *Registry) Close() error {
	r.closeOne.Do(func() { close(r.closeCh) })
	r.wg.Wait()
	return nil
}

// Upsert records an agent registration, merging it with anything already known.
func (r *Registry) Upsert(id string, reg protocol.AgentRegister) (*Device, error) {
	if id == "" {
		return nil, errors.New("allshare/registry: empty device id")
	}
	name := SanitizeName(reg.Name)
	if name == "" {
		name = "Windows PC"
	}
	paired := reg.PairedClients
	if len(paired) > MaxPairedClients {
		paired = paired[:MaxPairedClients]
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	d, ok := r.devices[id]
	if !ok {
		d = &Device{ID: id, FirstSeen: now}
		r.devices[id] = d
	}
	d.Name = name
	d.OS = truncate(reg.OS, 64)
	d.AgentVersion = truncate(reg.AgentVersion, 32)
	d.Wake = sanitizeWake(reg.Wake)
	d.PairedClients = append([]string(nil), paired...)
	d.LastSeen = now
	r.markDirty()

	out := *d
	out.PairedClients = append([]string(nil), d.PairedClients...)
	return &out, nil
}

// Touch records that a device was seen without changing its registration.
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[id]; ok {
		d.LastSeen = time.Now().UTC()
		r.markDirty()
	}
}

// UpdateWake replaces a device's wake capability.
func (r *Registry) UpdateWake(id string, wake protocol.WakeCapability) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[id]; ok {
		d.Wake = sanitizeWake(wake)
		d.LastSeen = time.Now().UTC()
		r.markDirty()
	}
}

// Get returns a copy of one device record.
func (r *Registry) Get(id string) (*Device, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := *d
	out.PairedClients = append([]string(nil), d.PairedClients...)
	return &out, nil
}

// ForClient lists every device that has registered the given client key,
// newest-seen first so the machine the user actually uses appears at the top.
func (r *Registry) ForClient(clientID string) []*Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Device
	for _, d := range r.devices {
		if d.IsPairedWith(clientID) {
			c := *d
			c.PairedClients = nil // never leak one client's key to another
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// PeersOnLAN returns other devices sharing a LAN key with the given device.
// It is how a sleeping machine gets a magic packet delivered by a neighbour.
func (r *Registry) PeersOnLAN(lanKey, excludeID string) []*Device {
	if lanKey == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Device
	for _, d := range r.devices {
		if d.ID == excludeID || d.Wake.LANKey != lanKey {
			continue
		}
		c := *d
		c.PairedClients = nil
		out = append(out, &c)
	}
	return out
}

// Forget removes a client from a device's pairing list. It is how "unpair this
// Chromebook" is reflected server-side; the agent removes it locally too.
func (r *Registry) Forget(deviceID, clientID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[deviceID]
	if !ok {
		return ErrNotFound
	}
	filtered := d.PairedClients[:0]
	for _, c := range d.PairedClients {
		if c != clientID {
			filtered = append(filtered, c)
		}
	}
	d.PairedClients = filtered
	r.markDirty()
	return nil
}

// Count reports how many devices are registered.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices)
}

// SanitizeName strips control characters from a user-supplied device name and
// bounds its length. Names reach the client UI and the server log, so they are
// never trusted as-is.
func SanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			continue
		}
		out = append(out, r)
		if len(out) >= MaxDeviceNameLen {
			break
		}
	}
	// Trim surrounding whitespace without pulling in strings just for this.
	start, end := 0, len(out)
	for start < end && (out[start] == ' ' || out[start] == '\t') {
		start++
	}
	for end > start && (out[end-1] == ' ' || out[end-1] == '\t') {
		end--
	}
	return string(out[start:end])
}

func sanitizeWake(w protocol.WakeCapability) protocol.WakeCapability {
	const maxAddrs = 8
	if len(w.MACAddresses) > maxAddrs {
		w.MACAddresses = w.MACAddresses[:maxAddrs]
	}
	if len(w.BroadcastAddrs) > maxAddrs {
		w.BroadcastAddrs = w.BroadcastAddrs[:maxAddrs]
	}
	w.Method = truncate(w.Method, 24)
	w.LANKey = truncate(w.LANKey, 64)
	w.Reason = truncate(w.Reason, 200)
	if w.EstimatedSeconds < 0 {
		w.EstimatedSeconds = 0
	}
	if w.EstimatedSeconds > 24*3600 {
		w.EstimatedSeconds = 24 * 3600
	}
	return w
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
