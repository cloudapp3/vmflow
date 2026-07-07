//go:build !windows

package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SelfPath returns the absolute path of the currently running executable,
// resolving any symlinks.
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil
	}
	return resolved, nil
}

// AtomicReplace replaces the binary at currentBinary with the new binary at
// newBinary. It writes to a temp file in the same directory and renames,
// which is atomic on Linux and macOS when on the same filesystem.
//
// The temp file uses a random, exclusive name (os.CreateTemp → O_CREAT|O_EXCL),
// so an attacker who can write to the directory cannot pre-plant it as a symlink
// and have us write the new binary through that symlink onto an arbitrary file.
func AtomicReplace(newBinary, currentBinary string) error {
	dir := filepath.Dir(currentBinary)

	src, err := os.Open(newBinary)
	if err != nil {
		return fmt.Errorf("cannot open new binary: %w", err)
	}
	defer src.Close()

	// Random, exclusive temp name: an attacker cannot predict/plant this path,
	// and O_EXCL means we never open (or write through) an existing entry.
	tmp, err := os.CreateTemp(dir, ".vmflow-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("cannot copy binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("cannot close temp file: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanup()
		return fmt.Errorf("cannot chmod temp file: %w", err)
	}

	// Defense-in-depth: make sure the path we rename is still the regular file
	// we wrote, not a symlink swapped in between CreateTemp and Rename in a
	// writable shared dir.
	if info, err := os.Lstat(tmpPath); err != nil || info.Mode().Type() != 0 {
		cleanup()
		return fmt.Errorf("temp file changed unexpectedly before rename")
	}

	if err := os.Rename(tmpPath, currentBinary); err != nil {
		cleanup()
		return fmt.Errorf("cannot replace binary (try running with appropriate privileges): %w", err)
	}

	return nil
}
