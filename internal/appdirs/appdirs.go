// Package appdirs resolves vmflow-owned configuration locations without tying
// them to the executable directory.
package appdirs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	ConfigFilename    = "config.yaml"
	ManagedMarkerName = ".vmflow-managed"

	LinuxSystemConfigPath  = "/etc/vmflow/config.yaml"
	DarwinSystemConfigPath = "/Library/Application Support/vmflow/config.yaml"
	WindowsProgramData     = `C:\ProgramData`
)

// Scope identifies whether configuration belongs to a user-run process or to
// the machine-wide native service.
type Scope uint8

const (
	User Scope = iota
	System
)

// ConfigPath returns the canonical config path for scope on the current OS.
func ConfigPath(scope Scope) (string, error) {
	return configPathFor(runtime.GOOS, scope, os.Getenv, os.UserHomeDir)
}

// UserConfigPath returns the config path used by vmflow run and user-scoped
// initialization.
func UserConfigPath() (string, error) {
	return ConfigPath(User)
}

// SystemConfigPath returns the config path used by the native system service.
func SystemConfigPath() (string, error) {
	return ConfigPath(System)
}

// IsCanonicalConfigPath reports whether candidate is the current machine's
// canonical path for scope. It is used before placing an ownership marker so a
// custom -config directory is never claimed by vmflow.
func IsCanonicalConfigPath(candidate string, scope Scope) bool {
	expected, err := ConfigPath(scope)
	if err != nil {
		return false
	}
	left := filepath.Clean(strings.TrimSpace(candidate))
	right := filepath.Clean(expected)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// ManagedMarkerPath returns the ownership marker beside a config file.
func ManagedMarkerPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ManagedMarkerName)
}

type getenvFunc func(string) string
type userHomeDirFunc func() (string, error)

func configPathFor(goos string, scope Scope, getenv getenvFunc, userHomeDir userHomeDirFunc) (string, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if userHomeDir == nil {
		userHomeDir = func() (string, error) { return "", errors.New("home directory is unavailable") }
	}

	switch scope {
	case System:
		switch goos {
		case "linux":
			return LinuxSystemConfigPath, nil
		case "darwin":
			return DarwinSystemConfigPath, nil
		case "windows":
			root := strings.TrimSpace(getenv("ProgramData"))
			if root == "" {
				root = WindowsProgramData
			}
			if !windowsPathIsAbs(root) {
				return "", fmt.Errorf("ProgramData must be an absolute path: %s", root)
			}
			return windowsJoin(root, "vmflow", ConfigFilename), nil
		default:
			return "", fmt.Errorf("system configuration is not supported on %s", goos)
		}
	case User:
		switch goos {
		case "linux":
			root := strings.TrimSpace(getenv("XDG_CONFIG_HOME"))
			if root != "" && !path.IsAbs(root) {
				root = ""
			}
			if root == "" {
				home, err := requiredHome(userHomeDir)
				if err != nil {
					return "", err
				}
				root = filepath.Join(home, ".config")
			}
			return filepath.Join(root, "vmflow", ConfigFilename), nil
		case "darwin":
			home, err := requiredHome(userHomeDir)
			if err != nil {
				return "", err
			}
			return filepath.Join(home, "Library", "Application Support", "vmflow", ConfigFilename), nil
		case "windows":
			root := strings.TrimSpace(getenv("LocalAppData"))
			if root == "" {
				home, err := requiredHome(userHomeDir)
				if err != nil {
					return "", err
				}
				root = windowsJoin(home, "AppData", "Local")
			}
			if !windowsPathIsAbs(root) {
				return "", fmt.Errorf("LocalAppData must be an absolute path: %s", root)
			}
			return windowsJoin(root, "vmflow", ConfigFilename), nil
		default:
			return "", fmt.Errorf("user configuration is not supported on %s", goos)
		}
	default:
		return "", fmt.Errorf("unknown configuration scope %d", scope)
	}
}

func requiredHome(resolve userHomeDirFunc) (string, error) {
	home, err := resolve()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	home = strings.TrimSpace(home)
	if home == "" {
		return "", errors.New("resolve user home directory: empty path")
	}
	return home, nil
}

func windowsJoin(root string, elements ...string) string {
	result := strings.TrimRight(strings.TrimSpace(root), `\/`)
	for _, element := range elements {
		element = strings.Trim(element, `\/`)
		if element != "" {
			result += `\` + element
		}
	}
	return result
}

func windowsPathIsAbs(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}
