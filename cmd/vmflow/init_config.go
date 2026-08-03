package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudapp3/vmflow/config"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/appdirs"
)

type managedConfigSave struct {
	Config        config.File
	ConfigCreated bool
	DirCreated    bool
	MarkerCreated bool
}

type managedConfigSaveError struct {
	err       error
	committed bool
}

func (err *managedConfigSaveError) Error() string { return err.err.Error() }
func (err *managedConfigSaveError) Unwrap() error { return err.err }

func managedSetupConfigCommitted(err error) bool {
	if controlapi.SetupConfigCommitted(err) {
		return true
	}
	var saveErr *managedConfigSaveError
	return errors.As(err, &saveErr) && saveErr.committed
}

func saveManagedSetupConfig(path string, scope appdirs.Scope, update controlapi.SetupConfigUpdate) (managedConfigSave, error) {
	dir := filepath.Dir(path)
	dirExisted, err := existingDirectory(dir)
	if err != nil {
		return managedConfigSave{}, err
	}
	return saveManagedSetupConfigWithClaim(path, scope, update, !dirExisted)
}

func saveManagedSetupConfigWithClaim(path string, scope appdirs.Scope, update controlapi.SetupConfigUpdate, claimManagedDirectory bool) (managedConfigSave, error) {
	dir := filepath.Dir(path)
	dirExisted, err := existingDirectory(dir)
	if err != nil {
		return managedConfigSave{}, err
	}
	configExisted, err := existingRegularOrMissing(path, "config")
	if err != nil {
		return managedConfigSave{}, err
	}
	if err := validateExistingManagedConfigMarker(path, scope); err != nil {
		return managedConfigSave{}, err
	}

	saved, err := controlapi.SaveSetupConfig(path, update)
	if err != nil {
		return managedConfigSave{}, err
	}
	state := managedConfigSave{
		Config:        saved,
		ConfigCreated: !configExisted,
		DirCreated:    !dirExisted,
	}
	markerCreated, err := ensureManagedConfigMarker(path, scope, claimManagedDirectory)
	state.MarkerCreated = markerCreated
	if err == nil {
		return state, nil
	}
	if state.ConfigCreated {
		if rollbackErr := rollbackManagedConfig(path, state); rollbackErr != nil {
			return state, &managedConfigSaveError{
				err:       fmt.Errorf("%w; rollback new config: %v", err, rollbackErr),
				committed: true,
			}
		}
		return state, err
	}
	return state, &managedConfigSaveError{err: err, committed: true}
}

func existingDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect config directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("config directory must be a directory, not a symlink: %s", path)
	}
	return true, nil
}

func existingRegularOrMissing(path, label string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s path %s: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s path must be a regular file, not a symlink: %s", label, path)
	}
	return true, nil
}

func ensureManagedConfigMarker(configPath string, scope appdirs.Scope, claimDirectory bool) (bool, error) {
	if !appdirs.IsCanonicalConfigPath(configPath, scope) {
		return false, nil
	}
	markerPath := appdirs.ManagedMarkerPath(configPath)
	markerExists, err := existingRegularOrMissing(markerPath, "managed marker")
	if err != nil {
		return false, err
	}
	if markerExists {
		raw, err := os.ReadFile(markerPath)
		if err != nil {
			return false, fmt.Errorf("read managed marker %s: %w", markerPath, err)
		}
		if strings.TrimSpace(string(raw)) != "vmflow" {
			return false, fmt.Errorf("managed marker has unexpected content: %s", markerPath)
		}
		return false, nil
	}
	// Do not claim a pre-existing directory that may contain operator files.
	if !claimDirectory {
		return false, nil
	}
	file, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create managed marker %s: %w", markerPath, err)
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(markerPath)
		}
	}()
	if _, err := file.WriteString("vmflow\n"); err != nil {
		return false, fmt.Errorf("write managed marker %s: %w", markerPath, err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync managed marker %s: %w", markerPath, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close managed marker %s: %w", markerPath, err)
	}
	removeOnError = false
	return true, nil
}

func validateExistingManagedConfigMarker(configPath string, scope appdirs.Scope) error {
	if !appdirs.IsCanonicalConfigPath(configPath, scope) {
		return nil
	}
	markerPath := appdirs.ManagedMarkerPath(configPath)
	markerExists, err := existingRegularOrMissing(markerPath, "managed marker")
	if err != nil || !markerExists {
		return err
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read managed marker %s: %w", markerPath, err)
	}
	if strings.TrimSpace(string(raw)) != "vmflow" {
		return fmt.Errorf("managed marker has unexpected content: %s", markerPath)
	}
	return nil
}

func rollbackManagedConfig(configPath string, state managedConfigSave) error {
	if !state.ConfigCreated {
		return nil
	}
	var problems []string
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		problems = append(problems, err.Error())
	}
	if state.MarkerCreated {
		markerPath := appdirs.ManagedMarkerPath(configPath)
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			problems = append(problems, err.Error())
		}
	}
	if state.DirCreated {
		if err := os.Remove(filepath.Dir(configPath)); err != nil && !os.IsNotExist(err) {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) != 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}
