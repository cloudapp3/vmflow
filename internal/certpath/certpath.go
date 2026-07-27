// Package certpath provides the filesystem boundary for persisted certificates.
// User-provided certificate identifiers are never used as path components.
package certpath

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxCertificateFileSize = 16 << 20
	maxMetadataFileSize    = 8 << 20
	metadataFilename       = "certs.json"
	acmeIndexFilename      = "acme-domains.json"
)

// Namespace selects one of the fixed certificate storage locations.
type Namespace uint8

const (
	ACME Namespace = iota
	Manual
)

type pairPaths struct {
	directory string
	cert      string
	key       string
}

// Normalize canonicalizes and validates a DNS certificate identifier. IP
// address certificates are accepted, and a wildcard is allowed only as the
// complete left-most label when allowWildcard is true. Internationalized
// names must be supplied in their ASCII A-label form.
func Normalize(raw string, allowWildcard bool) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", fmt.Errorf("certificate domain is empty")
	}
	if strings.IndexByte(value, 0) >= 0 || strings.ContainsAny(value, "/\\") {
		return "", fmt.Errorf("invalid certificate domain %q", raw)
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("certificate domain must use ASCII DNS form: %q", raw)
		}
	}

	value = strings.TrimSuffix(value, ".")
	if value == "" {
		return "", fmt.Errorf("certificate domain is empty")
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.String(), nil
	}

	wildcard := false
	if strings.HasPrefix(value, "*.") {
		if !allowWildcard {
			return "", fmt.Errorf("wildcard certificate domain is not allowed here: %q", raw)
		}
		wildcard = true
		value = strings.TrimPrefix(value, "*.")
	}
	if strings.Contains(value, "*") {
		return "", fmt.Errorf("invalid wildcard certificate domain %q", raw)
	}
	if value == "" || len(value) > 253 {
		return "", fmt.Errorf("invalid certificate domain length: %q", raw)
	}

	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", fmt.Errorf("invalid certificate domain label in %q", raw)
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || (ch == '-' && i > 0 && i < len(label)-1) {
				continue
			}
			return "", fmt.Errorf("invalid certificate domain label in %q", raw)
		}
	}

	if wildcard {
		value = "*." + value
	}
	if len(value) > 253 {
		return "", fmt.Errorf("invalid certificate domain length: %q", raw)
	}
	return value, nil
}

// NormalizeAll validates, canonicalizes, and de-duplicates domains while
// preserving their first-seen order.
func NormalizeAll(raw []string, allowWildcard bool) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("no domains specified")
	}
	result := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for index, value := range raw {
		domain, err := Normalize(value, allowWildcard)
		if err != nil {
			return nil, fmt.Errorf("domain %d: %w", index+1, err)
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		result = append(result, domain)
	}
	return result, nil
}

// ReadPair securely reads a certificate and private key. If only the legacy
// domain-named pair exists, it is migrated before being returned.
func ReadPair(cacheDir string, namespace Namespace, domain string) ([]byte, []byte, error) {
	paths, canonical, err := pathsFor(cacheDir, namespace, domain)
	if err != nil {
		return nil, nil, err
	}
	certExists, err := regularFileExists(paths.cert)
	if err != nil {
		return nil, nil, err
	}
	keyExists, err := regularFileExists(paths.key)
	if err != nil {
		return nil, nil, err
	}
	if !certExists && !keyExists {
		migrated, migrateErr := migrateLegacyPair(paths, canonical)
		if migrateErr != nil {
			return nil, nil, migrateErr
		}
		if !migrated {
			return nil, nil, os.ErrNotExist
		}
		certExists, keyExists = true, true
	}
	if !certExists || !keyExists {
		return nil, nil, fmt.Errorf("incomplete certificate pair for %s", canonical)
	}

	certPEM, err := readRegularFile(paths.cert, maxCertificateFileSize)
	if err != nil {
		return nil, nil, fmt.Errorf("read certificate for %s: %w", canonical, err)
	}
	keyPEM, err := readRegularFile(paths.key, maxCertificateFileSize)
	if err != nil {
		return nil, nil, fmt.Errorf("read private key for %s: %w", canonical, err)
	}
	return certPEM, keyPEM, nil
}

// WritePair atomically replaces the certificate and private-key files. Each
// file replacement is atomic; callers must still treat the two files as a pair.
func WritePair(cacheDir string, namespace Namespace, domain string, certPEM, keyPEM []byte) error {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return fmt.Errorf("certificate and private key must not be empty")
	}
	paths, canonical, err := pathsFor(cacheDir, namespace, domain)
	if err != nil {
		return err
	}
	if err := ensureDirectory(paths.directory); err != nil {
		return err
	}
	if err := atomicWriteFile(paths.key, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write private key for %s: %w", canonical, err)
	}
	if err := atomicWriteFile(paths.cert, certPEM, 0o644); err != nil {
		return fmt.Errorf("write certificate for %s: %w", canonical, err)
	}
	return nil
}

// RemovePair removes a persisted certificate pair without following symlinks.
func RemovePair(cacheDir string, namespace Namespace, domain string) error {
	paths, canonical, err := pathsFor(cacheDir, namespace, domain)
	if err != nil {
		return err
	}
	certExists, err := regularFileExists(paths.cert)
	if err != nil {
		return fmt.Errorf("inspect certificate for %s: %w", canonical, err)
	}
	keyExists, err := regularFileExists(paths.key)
	if err != nil {
		return fmt.Errorf("inspect private key for %s: %w", canonical, err)
	}
	if !certExists && !keyExists {
		return nil
	}
	if err := removeRegularFile(paths.cert); err != nil {
		return fmt.Errorf("remove certificate for %s: %w", canonical, err)
	}
	if err := removeRegularFile(paths.key); err != nil {
		return fmt.Errorf("remove private key for %s: %w", canonical, err)
	}
	return syncDirectory(paths.directory)
}

// ReadMetadata reads the fixed certificate metadata document.
func ReadMetadata(cacheDir string) ([]byte, error) {
	root, err := cleanRoot(cacheDir)
	if err != nil {
		return nil, err
	}
	return readRegularFile(filepath.Join(root, metadataFilename), maxMetadataFileSize)
}

// WriteMetadata atomically replaces the fixed certificate metadata document.
func WriteMetadata(cacheDir string, data []byte) error {
	return writeFixedDocument(cacheDir, metadataFilename, data)
}

// ReadACMEIndex reads the fixed ACME domain index used for eager cache loads.
func ReadACMEIndex(cacheDir string) ([]byte, error) {
	root, err := cleanRoot(cacheDir)
	if err != nil {
		return nil, err
	}
	return readRegularFile(filepath.Join(root, acmeIndexFilename), maxMetadataFileSize)
}

// WriteACMEIndex atomically replaces the fixed ACME domain index.
func WriteACMEIndex(cacheDir string, data []byte) error {
	return writeFixedDocument(cacheDir, acmeIndexFilename, data)
}

func writeFixedDocument(cacheDir, filename string, data []byte) error {
	root, err := cleanRoot(cacheDir)
	if err != nil {
		return err
	}
	if err := ensureDirectory(root); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(root, filename), data, 0o600)
}

func pathsFor(cacheDir string, namespace Namespace, domain string) (pairPaths, string, error) {
	canonical, err := Normalize(domain, true)
	if err != nil {
		return pairPaths{}, "", err
	}
	root, err := cleanRoot(cacheDir)
	if err != nil {
		return pairPaths{}, "", err
	}
	directory := root
	switch namespace {
	case ACME:
	case Manual:
		directory = filepath.Join(root, "manual")
	default:
		return pairPaths{}, "", fmt.Errorf("invalid certificate namespace")
	}
	if err := ensureContained(root, directory); err != nil {
		return pairPaths{}, "", err
	}

	digest := sha256.Sum256([]byte(canonical))
	stem := "d-" + hex.EncodeToString(digest[:])
	certPath := filepath.Join(directory, stem+".crt")
	keyPath := filepath.Join(directory, stem+".key")
	if err := ensureContained(directory, certPath); err != nil {
		return pairPaths{}, "", err
	}
	if err := ensureContained(directory, keyPath); err != nil {
		return pairPaths{}, "", err
	}
	return pairPaths{directory: directory, cert: certPath, key: keyPath}, canonical, nil
}

func cleanRoot(cacheDir string) (string, error) {
	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir == "" {
		return "", fmt.Errorf("certificate cache directory is empty")
	}
	absolute, err := filepath.Abs(cacheDir)
	if err != nil {
		return "", fmt.Errorf("resolve certificate cache directory: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func ensureContained(root, candidate string) error {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return fmt.Errorf("resolve certificate path: %w", err)
	}
	if relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("certificate path escapes its cache directory")
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect certificate directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("certificate directory must be a directory, not a symlink")
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("certificate path must be a regular file, not a symlink")
	}
	return true, nil
}

func readRegularFile(path string, maxSize int64) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("path must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("file exceeds %d bytes", maxSize)
	}
	return data, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := ensureDirectory(directory); err != nil {
		return err
	}

	var original os.FileInfo
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("target must be a regular file, not a symlink")
		}
		original = info
	} else if !os.IsNotExist(err) {
		return err
	}

	temporary, err := os.CreateTemp(directory, ".vmflow-cert-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}

	current, statErr := os.Lstat(path)
	switch {
	case original == nil && statErr == nil:
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("target appeared while saving")
	case original == nil && !os.IsNotExist(statErr):
		_ = os.Remove(temporaryPath)
		return statErr
	case original != nil && statErr != nil:
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("target changed while saving")
	case original != nil && (!current.Mode().IsRegular() || !os.SameFile(original, current)):
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("target changed while saving")
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return syncDirectory(directory)
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("target must be a regular file, not a symlink")
	}
	return os.Remove(path)
}

func migrateLegacyPair(paths pairPaths, canonical string) (bool, error) {
	entries, err := os.ReadDir(paths.directory)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var certName, keyName string
	for _, entry := range entries {
		name := entry.Name()
		var suffix string
		switch {
		case strings.HasSuffix(name, ".crt"):
			suffix = ".crt"
		case strings.HasSuffix(name, ".key"):
			suffix = ".key"
		default:
			continue
		}
		legacyDomain, normalizeErr := Normalize(strings.TrimSuffix(name, suffix), true)
		if normalizeErr != nil || legacyDomain != canonical {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return false, fmt.Errorf("legacy certificate path for %s is not a regular file", canonical)
		}
		if suffix == ".crt" {
			if certName != "" {
				return false, fmt.Errorf("multiple legacy certificates found for %s", canonical)
			}
			certName = name
		} else {
			if keyName != "" {
				return false, fmt.Errorf("multiple legacy private keys found for %s", canonical)
			}
			keyName = name
		}
	}
	if certName == "" && keyName == "" {
		return false, nil
	}
	if certName == "" || keyName == "" {
		return false, fmt.Errorf("incomplete legacy certificate pair for %s", canonical)
	}

	legacyCertPath := filepath.Join(paths.directory, certName)
	legacyKeyPath := filepath.Join(paths.directory, keyName)
	certPEM, err := readRegularFile(legacyCertPath, maxCertificateFileSize)
	if err != nil {
		return false, err
	}
	keyPEM, err := readRegularFile(legacyKeyPath, maxCertificateFileSize)
	if err != nil {
		return false, err
	}
	if err := atomicWriteFile(paths.key, keyPEM, 0o600); err != nil {
		return false, err
	}
	if err := atomicWriteFile(paths.cert, certPEM, 0o644); err != nil {
		return false, err
	}
	if err := removeRegularFile(legacyCertPath); err != nil {
		return false, err
	}
	if err := removeRegularFile(legacyKeyPath); err != nil {
		return false, err
	}
	if err := syncDirectory(paths.directory); err != nil {
		return false, err
	}
	return true, nil
}

func syncDirectory(path string) error {
	return syncCertificateDirectory(path)
}
