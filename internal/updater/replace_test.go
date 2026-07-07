//go:build !windows

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("the-new-binary-bytes")
	newBin := filepath.Join(dir, "new")
	if err := os.WriteFile(newBin, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "vmflow")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(newBin, target); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("target content = %q, want %q", got, payload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target perms = %v, want 0755", info.Mode().Perm())
	}
	// No leftover temp files in the install directory.
	if matches, _ := filepath.Glob(filepath.Join(dir, ".vmflow-update-*")); len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

// TestAtomicReplaceResistsPlantedSymlink reproduces the original attack
// (predictable fixed temp name ".vmflow-update-tmp" written via OpenFile, which
// followed a planted symlink) and asserts the victim file is NOT clobbered.
// It fails on the old implementation and passes on the randomized one.
func TestAtomicReplaceResistsPlantedSymlink(t *testing.T) {
	dir := t.TempDir()

	victim := filepath.Join(dir, "victim")
	original := []byte("victim-contents")
	if err := os.WriteFile(victim, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// Attacker plants a symlink at the OLD fixed temp name -> victim.
	if err := os.Symlink(victim, filepath.Join(dir, ".vmflow-update-tmp")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	payload := []byte("new-binary-bytes")
	newBin := filepath.Join(dir, "new")
	if err := os.WriteFile(newBin, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "vmflow")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := AtomicReplace(newBin, target); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("victim clobbered via planted symlink: got %q, want %q", got, original)
	}
	tb, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(tb) != string(payload) {
		t.Fatalf("target content = %q, want %q", tb, payload)
	}
}
