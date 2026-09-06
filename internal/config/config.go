// Package config holds the agent's on-disk state: its settings, its identity
// and the list of clients it has paired with.
//
// The paired-client list is the agent's own security boundary. The rendezvous
// keeps a copy so it knows what to route, but the agent never trusts that copy:
// a connection is accepted only if the client's key is in *this* file. That
// means a compromised rendezvous cannot introduce a new client to a PC.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// DefaultRendezvous is used when nothing else is configured. It is empty on
// purpose: ALL SHARE has no built-in home to phone, and the installer or the
// user supplies the address of a service they control.
const DefaultRendezvous = ""

// PairedClient is one device this PC has agreed to accept.
type PairedClient struct {
	// ID is the client's Ed25519 public key, base64url encoded. Every
	// signalling payload from this client is verified against it.
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	PairedAt time.Time `json:"pairedAt"`
	LastSeen time.Time `json:"lastSeen"`
}

// Config is the agent's persisted configuration.
type Config struct {
	// DeviceName is what the user sees in their client. Chosen at install time
	// from the machine name, and editable.
	DeviceName string `json:"deviceName"`
	Rendezvous string `json:"rendezvous"`

	PairedClients []PairedClient `json:"pairedClients"`

	// Streaming defaults. A client can change these per session; these are
	// where a session starts.
	MaxBitrateKbps   int    `json:"maxBitrateKbps"`
	MinBitrateKbps   int    `json:"minBitrateKbps"`
	StartBitrateKbps int    `json:"startBitrateKbps"`
	MaxFPS           int    `json:"maxFps"`
	Preset           string `json:"preset"`

	AudioEnabled     bool `json:"audioEnabled"`
	ClipboardToPC    bool `json:"clipboardToPc"`
	ClipboardFromPC  bool `json:"clipboardFromPc"`
	LocalCursor      bool `json:"localCursor"`
	AllowMultiClient bool `json:"allowMultiClient"`

	// RequireApproval makes a first connection from each paired device wait for
	// someone at the PC to accept it.
	RequireApproval bool `json:"requireApproval"`

	// Wake settings.
	WakeCheckInMinutes int  `json:"wakeCheckInMinutes"`
	WakeEnabled        bool `json:"wakeEnabled"`
	AllowLANWakeHelp   bool `json:"allowLanWakeHelp"`

	// Diagnostics.
	LogLevel string `json:"logLevel"`

	path string
	mu   sync.Mutex
}

// Defaults returns a configuration suitable for a fresh install.
func Defaults() Config {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "Windows PC"
	}
	return Config{
		DeviceName:       name,
		Rendezvous:       DefaultRendezvous,
		MaxBitrateKbps:   25_000,
		MinBitrateKbps:   600,
		StartBitrateKbps: 6_000,
		MaxFPS:           60,
		Preset:           "balanced",
		AudioEnabled:     true,
		ClipboardToPC:    true,
		ClipboardFromPC:  true,
		LocalCursor:      true,
		AllowMultiClient: false,
		RequireApproval:  false,
		// Fifteen minutes is a deliberate compromise. Shorter wakes the machine
		// more often for nothing; longer makes "Wake PC" feel broken. The user
		// can change it, and the client shows the real interval rather than
		// implying an instant wake.
		WakeCheckInMinutes: 15,
		WakeEnabled:        true,
		AllowLANWakeHelp:   true,
		LogLevel:           "info",
	}
}

// Dir returns the directory holding the agent's state.
//
// On Windows this is under ProgramData so the SYSTEM service and the desktop
// helper share one copy. Elsewhere it follows the usual per-user convention,
// which is where a development or test run should keep its files.
func Dir() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "ALL SHARE"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("allshare/config: locate configuration directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "allshare"), nil
}

// Load reads the configuration at path, filling in defaults for anything absent.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	cfg.path = path

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("allshare/config: read %s: %w", path, err)
	}
	// Unmarshalling over the defaults means a config written by an older
	// version gains new fields with sensible values instead of zeros.
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("allshare/config: parse %s: %w", path, err)
	}
	cfg.path = path
	cfg.normalize()
	return &cfg, nil
}

func (c *Config) normalize() {
	if c.MaxFPS <= 0 || c.MaxFPS > 240 {
		c.MaxFPS = 60
	}
	if c.MaxBitrateKbps < 500 {
		c.MaxBitrateKbps = 25_000
	}
	if c.MinBitrateKbps < 100 || c.MinBitrateKbps > c.MaxBitrateKbps {
		c.MinBitrateKbps = 600
	}
	if c.StartBitrateKbps < c.MinBitrateKbps || c.StartBitrateKbps > c.MaxBitrateKbps {
		c.StartBitrateKbps = minInt(6_000, c.MaxBitrateKbps)
	}
	if c.WakeCheckInMinutes < 0 {
		c.WakeCheckInMinutes = 0
	}
	if c.WakeCheckInMinutes > 12*60 {
		c.WakeCheckInMinutes = 12 * 60
	}
	if c.DeviceName == "" {
		c.DeviceName = "Windows PC"
	}
}

// Save writes the configuration atomically.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	if c.path == "" {
		return errors.New("allshare/config: no path set")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("allshare/config: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("allshare/config: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".config-*")
	if err != nil {
		return fmt.Errorf("allshare/config: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/config: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("allshare/config: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("allshare/config: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("allshare/config: chmod: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("allshare/config: install: %w", err)
	}
	return nil
}

// IsPaired reports whether a client key is trusted by this PC.
func (c *Config) IsPaired(clientID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range c.PairedClients {
		if client.ID == clientID {
			return true
		}
	}
	return false
}

// AddPairedClient records a newly paired device and persists it immediately.
//
// Persisting straight away matters: a crash between pairing and the next save
// would leave the user with a client that says it is paired and a PC that
// disagrees.
func (c *Config) AddPairedClient(id, label string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	for i := range c.PairedClients {
		if c.PairedClients[i].ID == id {
			c.PairedClients[i].Label = label
			c.PairedClients[i].PairedAt = now
			return c.saveLocked()
		}
	}
	const maxClients = 64
	if len(c.PairedClients) >= maxClients {
		return fmt.Errorf("allshare/config: this PC is already paired with %d devices", maxClients)
	}
	c.PairedClients = append(c.PairedClients, PairedClient{ID: id, Label: label, PairedAt: now})
	return c.saveLocked()
}

// RemovePairedClient forgets a device.
func (c *Config) RemovePairedClient(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	filtered := c.PairedClients[:0]
	for _, client := range c.PairedClients {
		if client.ID != id {
			filtered = append(filtered, client)
		}
	}
	c.PairedClients = filtered
	return c.saveLocked()
}

// TouchClient records that a paired device connected.
func (c *Config) TouchClient(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.PairedClients {
		if c.PairedClients[i].ID == id {
			c.PairedClients[i].LastSeen = time.Now().UTC()
			_ = c.saveLocked()
			return
		}
	}
}

// PairedIDs returns the trusted client keys, for registration.
func (c *Config) PairedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.PairedClients))
	for _, client := range c.PairedClients {
		out = append(out, client.ID)
	}
	return out
}

// ClientLabel returns the friendly name recorded for a paired client.
func (c *Config) ClientLabel(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range c.PairedClients {
		if client.ID == id {
			return client.Label
		}
	}
	return ""
}

// Update applies a mutation under the lock and saves.
func (c *Config) Update(mutate func(*Config)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	mutate(c)
	c.normalize()
	return c.saveLocked()
}

// Path reports where this configuration is stored.
func (c *Config) Path() string { return c.path }

// SetPath sets the destination for Save, used when creating a fresh config.
func (c *Config) SetPath(path string) { c.path = path }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
