package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/appdirs"
)

func TestSaveManagedSetupConfigMarksNewCanonicalUserDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	state, err := saveManagedSetupConfig(path, appdirs.User, controlapi.SetupConfigUpdate{Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.ConfigCreated || !state.DirCreated || !state.MarkerCreated {
		t.Fatalf("save state = %+v", state)
	}
	raw, err := os.ReadFile(appdirs.ManagedMarkerPath(path))
	if err != nil || string(raw) != "vmflow\n" {
		t.Fatalf("managed marker = %q, %v", raw, err)
	}
}

func TestSaveManagedSetupConfigDoesNotClaimCustomOrExistingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canonical bool
	}{
		{name: "custom path"},
		{name: "existing canonical directory", canonical: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			path := filepath.Join(root, "custom", "config.yaml")
			if tc.canonical {
				var err error
				path, err = appdirs.UserConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			state, err := saveManagedSetupConfig(path, appdirs.User, controlapi.SetupConfigUpdate{Language: "en"})
			if err != nil {
				t.Fatal(err)
			}
			if state.MarkerCreated {
				t.Fatalf("custom/pre-existing directory was claimed: %+v", state)
			}
			if _, err := os.Stat(appdirs.ManagedMarkerPath(path)); !os.IsNotExist(err) {
				t.Fatalf("unexpected managed marker: %v", err)
			}
		})
	}
}

func TestSaveManagedSetupConfigRejectsInvalidMarkerBeforeChangingExistingConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("version: 1\nlanguage: en\nrules: []\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appdirs.ManagedMarkerPath(path), []byte("not-vmflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := saveManagedSetupConfig(path, appdirs.User, controlapi.SetupConfigUpdate{Language: "zh-CN"}); err == nil {
		t.Fatal("invalid marker was accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("config changed before marker validation:\n%s", raw)
	}
}

func TestRollbackManagedConfigRemovesOnlyNewArtifacts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := appdirs.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	state, err := saveManagedSetupConfig(path, appdirs.User, controlapi.SetupConfigUpdate{Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackManagedConfig(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("new managed directory still exists: %v", err)
	}
}
