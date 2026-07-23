package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudapp3/vmflow/internal/clientconfig"
)

func TestLoadManagementDefaultsUsesProfileAndEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := clientconfig.Save(path, clientconfig.Profile{
		Address: "http://127.0.0.1:19123", Token: "profile-token", ConfigPath: configPath,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", path)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")
	got := loadManagementDefaults(&bytes.Buffer{})
	if got.Address != "http://127.0.0.1:19123" || got.Token != "profile-token" || got.ConfigPath != configPath {
		t.Fatalf("defaults = %+v", got)
	}

	t.Setenv("VMFLOW_CONTROL_TOKEN", "env-token")
	t.Setenv("VMFLOW_CONTROL_ADDR", "http://127.0.0.1:19222")
	got = loadManagementDefaults(&bytes.Buffer{})
	if got.Address != "http://127.0.0.1:19222" || got.Token != "env-token" {
		t.Fatalf("environment did not override profile: %+v", got)
	}
}

func TestLoadManagementDefaultsWarnsAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VMFLOW_CLIENT_CONFIG", path)
	t.Setenv("VMFLOW_CONTROL_TOKEN", "")
	t.Setenv("VMFLOW_CONTROL_ADDR", "")
	var output bytes.Buffer
	got := loadManagementDefaults(&output)
	if got.Address != "http://127.0.0.1:19090" || got.Token != "" || output.Len() == 0 {
		t.Fatalf("fallback = %+v, warning = %q", got, output.String())
	}
}
