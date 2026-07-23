// Package clientconfig stores the local management credentials used by the
// bundled CLI and TUI. The profile is deliberately limited to loopback
// management addresses because vmflow's control listener is local-only.
package clientconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const profileVersion = 1

// Profile identifies one local daemon and the bearer token used to manage it.
type Profile struct {
	Version    int    `yaml:"version"`
	Address    string `yaml:"address"`
	Token      string `yaml:"token"`
	ConfigPath string `yaml:"config_path,omitempty"`
}

// DefaultPath returns the client profile path. VMFLOW_CLIENT_CONFIG is mainly
// useful for isolated deployments and tests; relative overrides are rejected.
func DefaultPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("VMFLOW_CLIENT_CONFIG")); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("VMFLOW_CLIENT_CONFIG must be an absolute path")
		}
		return filepath.Clean(override), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "vmflow", "client.yaml"), nil
}

// LoadDefault loads the default profile and returns its resolved path.
func LoadDefault() (Profile, string, error) {
	path, err := DefaultPath()
	if err != nil {
		return Profile{}, "", err
	}
	profile, err := Load(path)
	return profile, path, err
}

// Load reads and validates a client profile without following a symlink at the
// final path component.
func Load(path string) (Profile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Profile{}, fmt.Errorf("missing client profile path")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return Profile{}, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return Profile{}, fmt.Errorf("client profile must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && pathInfo.Mode().Perm()&0o077 != 0 {
		return Profile{}, fmt.Errorf("client profile permissions must be 0600 or stricter")
	}

	file, err := os.Open(path)
	if err != nil {
		return Profile{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return Profile{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return Profile{}, fmt.Errorf("client profile changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return Profile{}, err
	}
	if len(raw) > 64*1024 {
		return Profile{}, fmt.Errorf("client profile is too large")
	}
	var profile Profile
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("parse client profile: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Profile{}, fmt.Errorf("multiple YAML documents are not supported")
		}
		return Profile{}, fmt.Errorf("parse client profile: %w", err)
	}
	if err := validate(&profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// Save atomically writes a client profile with mode 0600.
func Save(path string, profile Profile) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("missing client profile path")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("client profile path must be absolute")
	}
	profile.Version = profileVersion
	if err := validate(&profile); err != nil {
		return err
	}
	raw, err := yaml.Marshal(profile)
	if err != nil {
		return fmt.Errorf("encode client profile: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create client profile directory: %w", err)
	}
	var original os.FileInfo
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("client profile must be a regular file, not a symlink")
		}
		original = info
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect client profile: %w", statErr)
	}

	temp, err := os.CreateTemp(dir, ".vmflow-client-*")
	if err != nil {
		return fmt.Errorf("create temporary client profile: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure temporary client profile: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		cleanup()
		return fmt.Errorf("write temporary client profile: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary client profile: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close temporary client profile: %w", err)
	}

	current, statErr := os.Lstat(path)
	switch {
	case original == nil && statErr == nil:
		_ = os.Remove(tempPath)
		return fmt.Errorf("client profile appeared while saving")
	case original == nil && !os.IsNotExist(statErr):
		_ = os.Remove(tempPath)
		return fmt.Errorf("reinspect client profile: %w", statErr)
	case original != nil && statErr != nil:
		_ = os.Remove(tempPath)
		return fmt.Errorf("client profile changed while saving")
	case original != nil && (!current.Mode().IsRegular() || !os.SameFile(original, current)):
		_ = os.Remove(tempPath)
		return fmt.Errorf("client profile changed while saving")
	}
	if err := replaceProfileFile(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace client profile: %w", err)
	}
	if err := syncProfileDirectory(dir); err != nil {
		return fmt.Errorf("sync client profile directory: %w", err)
	}
	return nil
}

func validate(profile *Profile) error {
	if profile == nil {
		return fmt.Errorf("missing client profile")
	}
	if profile.Version == 0 {
		profile.Version = profileVersion
	}
	if profile.Version != profileVersion {
		return fmt.Errorf("unsupported client profile version: %d", profile.Version)
	}
	profile.Address = strings.TrimRight(strings.TrimSpace(profile.Address), "/")
	profile.Token = strings.TrimSpace(profile.Token)
	profile.ConfigPath = strings.TrimSpace(profile.ConfigPath)
	if profile.Token == "" {
		return fmt.Errorf("client profile token is empty")
	}
	if profile.ConfigPath != "" && !filepath.IsAbs(profile.ConfigPath) {
		return fmt.Errorf("client profile config_path must be absolute")
	}
	parsed, err := url.Parse(profile.Address)
	if err != nil {
		return fmt.Errorf("invalid client profile address: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("client profile address must use http or https")
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("client profile address must not contain credentials, a path, query, or fragment")
	}
	host := parsed.Hostname()
	if host == "" || !(strings.EqualFold(host, "localhost") || isLoopbackIP(host)) {
		return fmt.Errorf("client profile address must use a loopback host")
	}
	port := parsed.Port()
	if port == "" {
		return fmt.Errorf("client profile address must include a port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("client profile address port must be between 1 and 65535")
	}
	return nil
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
