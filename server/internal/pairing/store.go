// Package pairing tracks the short-lived pairing windows an agent opens.
//
// The server is a blind post box here. It stores the agent's public half of an
// X25519 exchange under a slow-KDF-derived handle and hands it to whoever
// presents the matching handle. It never sees the pairing code, cannot derive
// it from anything it stores, and cannot forge the key confirmation that both
// peers check before trusting each other.
package pairing

import (
	"errors"
	"sync"
	"time"

	"github.com/mmc/all-share/shared/pair"
	"github.com/mmc/all-share/shared/protocol"
)

// Errors returned by the store.
var (
	ErrNoWindow = errors.New("allshare/pairing: no open pairing window for that code")
	ErrConsumed = errors.New("allshare/pairing: pairing window already used")
	ErrTooMany  = errors.New("allshare/pairing: too many open pairing windows")
)

// MaxOpenWindows bounds concurrent pairing windows across the whole server.
// Windows are user-initiated and last three minutes, so this is generous for a
// personal deployment while still capping a flood.
const MaxOpenWindows = 256

// Window is one agent's open invitation to pair.
type Window struct {
	CodeID       string
	AgentID      string
	Salt         string
	EphemeralPub string
	IdentityPub  string
	DeviceName   string
	ExpiresAt    time.Time

	// consumed marks a window that has already had a confirmation attempt.
	// A pairing code gets exactly one attempt: allowing retries would turn an
	// infeasible offline attack into a feasible online guessing game.
	consumed bool
}

// Store holds open pairing windows.
type Store struct {
	mu      sync.Mutex
	byCode  map[string]*Window
	byAgent map[string]string
	now     func() time.Time
}

// NewStore constructs an empty pairing store.
func NewStore() *Store {
	return &Store{
		byCode:  map[string]*Window{},
		byAgent: map[string]string{},
		now:     time.Now,
	}
}

// Publish opens a pairing window on behalf of an agent, replacing any window
// that agent already had open.
func (s *Store) Publish(agentID string, p protocol.PairPublish) (*Window, error) {
	if p.CodeID == "" || p.Salt == "" || p.EphemeralPub == "" || p.IdentityPub == "" {
		return nil, errors.New("allshare/pairing: incomplete pairing window")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()

	if prev, ok := s.byAgent[agentID]; ok {
		delete(s.byCode, prev)
	}
	if len(s.byCode) >= MaxOpenWindows {
		return nil, ErrTooMany
	}

	expires := time.Unix(p.ExpiresAt, 0)
	maxExpiry := s.now().Add(pair.Window)
	if p.ExpiresAt == 0 || expires.After(maxExpiry) {
		expires = maxExpiry
	}

	w := &Window{
		CodeID:       p.CodeID,
		AgentID:      agentID,
		Salt:         p.Salt,
		EphemeralPub: p.EphemeralPub,
		IdentityPub:  p.IdentityPub,
		DeviceName:   p.DeviceName,
		ExpiresAt:    expires,
	}
	s.byCode[p.CodeID] = w
	s.byAgent[agentID] = p.CodeID
	return w, nil
}

// Lookup returns an open window for a code handle.
func (s *Store) Lookup(codeID string) (*Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()

	w, ok := s.byCode[codeID]
	if !ok {
		return nil, ErrNoWindow
	}
	if w.consumed {
		return nil, ErrConsumed
	}
	c := *w
	return &c, nil
}

// Consume marks a window as used and returns it. A window can be consumed
// exactly once, whether the confirmation that follows succeeds or fails.
func (s *Store) Consume(codeID string) (*Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()

	w, ok := s.byCode[codeID]
	if !ok {
		return nil, ErrNoWindow
	}
	if w.consumed {
		return nil, ErrConsumed
	}
	w.consumed = true
	c := *w
	return &c, nil
}

// Revoke closes an agent's window, for example when the user cancels the dialog.
func (s *Store) Revoke(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if codeID, ok := s.byAgent[agentID]; ok {
		delete(s.byCode, codeID)
		delete(s.byAgent, agentID)
	}
}

// Open reports how many windows are currently open.
func (s *Store) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return len(s.byCode)
}

func (s *Store) gcLocked() {
	now := s.now()
	for code, w := range s.byCode {
		if now.After(w.ExpiresAt) {
			delete(s.byCode, code)
			if s.byAgent[w.AgentID] == code {
				delete(s.byAgent, w.AgentID)
			}
		}
	}
}
