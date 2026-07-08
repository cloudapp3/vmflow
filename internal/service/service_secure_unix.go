//go:build linux || darwin

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func validateTrustedServiceBinary(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service binary %s is not trusted: binary is not executable", path)
	}
	if err := validateUnixOwnedByRoot(path, path, "binary", info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("service binary %s is not trusted: binary is writable by group or others", path)
	}

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("service binary %s is not trusted: stat parent directory %s: %w", path, dir, err)
		}
		if !dirInfo.IsDir() {
			return fmt.Errorf("service binary %s is not trusted: parent path %s is not a directory", path, dir)
		}
		if err := validateUnixOwnedByRoot(path, dir, "parent directory", dirInfo); err != nil {
			return err
		}
		if dirInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("service binary %s is not trusted: parent directory %s is writable by group or others", path, dir)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return nil
}

func validateUnixOwnedByRoot(binaryPath, path, kind string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("service binary %s is not trusted: cannot inspect owner for %s %s", binaryPath, kind, path)
	}
	if stat.Uid != 0 {
		return fmt.Errorf("service binary %s is not trusted: %s %s must be owned by root", binaryPath, kind, path)
	}
	return nil
}
