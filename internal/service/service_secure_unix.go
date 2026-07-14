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
	return validateTrustedUnixServicePath(path, "binary", info)
}

func validateTrustedServiceConfig(path string, info os.FileInfo) error {
	return validateTrustedUnixServicePath(path, "config", info)
}

func validateTrustedUnixServicePath(path, kind string, info os.FileInfo) error {
	if err := validateUnixOwnedByRoot(path, kind, path, "file", info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("service %s %s is not trusted: file is writable by group or others", kind, path)
	}

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("service %s %s is not trusted: stat parent directory %s: %w", kind, path, dir, err)
		}
		if !dirInfo.IsDir() {
			return fmt.Errorf("service %s %s is not trusted: parent path %s is not a directory", kind, path, dir)
		}
		if err := validateUnixOwnedByRoot(path, kind, dir, "parent directory", dirInfo); err != nil {
			return err
		}
		if dirInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("service %s %s is not trusted: parent directory %s is writable by group or others", kind, path, dir)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return nil
}

func validateUnixOwnedByRoot(servicePath, serviceKind, path, objectKind string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("service %s %s is not trusted: cannot inspect owner for %s %s", serviceKind, servicePath, objectKind, path)
	}
	if stat.Uid != 0 {
		return fmt.Errorf("service %s %s is not trusted: %s %s must be owned by root", serviceKind, servicePath, objectKind, path)
	}
	return nil
}
