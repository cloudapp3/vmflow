//go:build windows

package updater

import "errors"

// SelfUpdateSupported reports whether the running platform can replace its own
// executable in place.
func SelfUpdateSupported() bool { return false }

// SelfPath returns an error on Windows.
func SelfPath() (string, error) {
	return "", errors.New("self-update is not supported on Windows")
}

// AtomicReplace returns an error on Windows.
func AtomicReplace(_, _ string) error {
	return errors.New("self-update is not supported on Windows")
}
