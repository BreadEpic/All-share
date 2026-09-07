package config

import (
	"path/filepath"
	"testing"
)

// Removing a paired device is the recovery path for a lost or stolen client, so
// it has to be final and it has to survive a restart. A device that comes back
// after the agent is restarted would make the whole procedure worthless.
func TestRemovePairedClientIsFinalAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.SetPath(path)

	const lost = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	const kept = "__________________________________________8"
	if err := cfg.AddPairedClient(lost, "Stolen Chromebook"); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := cfg.AddPairedClient(kept, "Phone"); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if !cfg.IsPaired(lost) {
		t.Fatal("a freshly paired device is not recognised")
	}

	if err := cfg.RemovePairedClient(lost); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if cfg.IsPaired(lost) {
		t.Error("a forgotten device is still accepted")
	}
	if !cfg.IsPaired(kept) {
		t.Error("forgetting one device removed another")
	}

	// Reload from disk: this is what a service restart does.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.IsPaired(lost) {
		t.Error("a forgotten device came back after a restart")
	}
	if !reloaded.IsPaired(kept) {
		t.Error("an untouched device was lost across a restart")
	}
	if len(reloaded.PairedIDs()) != 1 {
		t.Errorf("expected 1 paired device after the removal, got %d", len(reloaded.PairedIDs()))
	}
}

// Removing something that was never there is not an error: a user retrying the
// command should not be told they did something wrong.
func TestRemovePairedClientIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.SetPath(path)
	if err := cfg.RemovePairedClient("not-a-device"); err != nil {
		t.Errorf("removing an unknown device reported an error: %v", err)
	}
}
