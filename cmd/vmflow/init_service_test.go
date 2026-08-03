package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/service"
)

func TestRunInitServiceInstallsAndVerifiesBackgroundService(t *testing.T) {
	plan := service.InstallPlan{
		BinaryPath:  "/usr/local/bin/vmflow",
		ConfigPath:  "/etc/vmflow/config.yaml",
		ServiceName: "vmflow",
	}
	result := initResult{
		ConfigPath:     plan.ConfigPath,
		ControlAddress: "http://127.0.0.1:19090",
		Language:       i18n.SimplifiedChinese,
		RunMode:        initRunModeService,
		ServicePlan:    plan,
	}
	installed := false
	inspected := false
	ready := false
	var output bytes.Buffer
	err := runInitServiceWithOps(result, &output, i18n.New(i18n.SimplifiedChinese), initServiceOps{
		elevated: true,
		install: func(payload initServicePayload, output io.Writer) error {
			installed = true
			if payload.BinaryPath != plan.BinaryPath || payload.ConfigPath != plan.ConfigPath || payload.ServiceName != plan.ServiceName {
				t.Fatalf("service payload = %+v", payload)
			}
			return nil
		},
		inspect: func(cfg service.Config) (service.Summary, error) {
			inspected = true
			return service.Summary{Installed: true, Running: true, Enabled: true, State: "active"}, nil
		},
		waitReady: func(address string) error {
			ready = true
			if address != result.ControlAddress {
				t.Fatalf("ready address = %q", address)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runInitServiceWithOps: %v", err)
	}
	if !installed || !inspected || !ready {
		t.Fatalf("calls: install=%t inspect=%t ready=%t", installed, inspected, ready)
	}
	text := output.String()
	for _, want := range []string{
		"vmflow 已在后台运行",
		"系统服务：vmflow（运行中，已启用开机启动）",
		"配置范围：系统服务",
		"配置：/etc/vmflow/config.yaml",
		"管理地址: http://127.0.0.1:19090",
		"管理界面：vmflow tui",
		"journalctl -u vmflow",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("service success output missing %q:\n%s", want, text)
		}
	}
}

func TestRunInitServiceUsesElevatedHandoff(t *testing.T) {
	plan := service.InstallPlan{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"}
	elevatedCalls := 0
	err := runInitServiceWithOps(initResult{
		ConfigPath: plan.ConfigPath, ControlAddress: "http://127.0.0.1:19090", ServicePlan: plan,
	}, io.Discard, i18n.New(i18n.English), initServiceOps{
		runElevated: func(got initServicePayload, locale string, _ io.Writer) error {
			elevatedCalls++
			if got.BinaryPath != plan.BinaryPath || got.ConfigPath != plan.ConfigPath || got.ServiceName != plan.ServiceName || locale != i18n.English {
				t.Fatalf("elevated args = %+v, %q", got, locale)
			}
			return nil
		},
		inspect: func(service.Config) (service.Summary, error) {
			return service.Summary{Installed: true, Running: true, Enabled: true}, nil
		},
		waitReady: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("runInitServiceWithOps: %v", err)
	}
	if elevatedCalls != 1 {
		t.Fatalf("elevated calls = %d", elevatedCalls)
	}
}

func TestRunInitServiceRejectsIncompleteServiceState(t *testing.T) {
	err := runInitServiceWithOps(initResult{
		ControlAddress: "http://127.0.0.1:19090",
		ServicePlan:    service.InstallPlan{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"},
	}, io.Discard, i18n.New(i18n.English), initServiceOps{
		elevated: true,
		install:  func(initServicePayload, io.Writer) error { return nil },
		inspect: func(service.Config) (service.Summary, error) {
			return service.Summary{Installed: true, Running: true, Enabled: false, State: "active"}, nil
		},
		waitReady: func(string) error {
			t.Fatal("readiness should not run for a disabled service")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "enabled=false") {
		t.Fatalf("service state error = %v", err)
	}
}

func TestRunInitServiceStopsAfterInstallFailure(t *testing.T) {
	wantErr := errors.New("systemctl restart failed")
	err := runInitServiceWithOps(initResult{
		ServicePlan: service.InstallPlan{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/etc/vmflow/config.yaml", ServiceName: "vmflow"},
	}, io.Discard, i18n.New(i18n.English), initServiceOps{
		elevated: true,
		install:  func(initServicePayload, io.Writer) error { return wantErr },
		inspect: func(service.Config) (service.Summary, error) {
			t.Fatal("inspect should not run after install failure")
			return service.Summary{}, nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("install error = %v", err)
	}
}

func TestPrintInitServiceFailureKeepsSavedConfigActionable(t *testing.T) {
	var output bytes.Buffer
	printInitServiceFailure(&output, initResult{ConfigPath: "/etc/vmflow/config.yaml"}, i18n.New(i18n.SimplifiedChinese), savedInitServiceError(errors.New("权限不足")))
	text := output.String()
	for _, want := range []string{"配置已保存", "/etc/vmflow/config.yaml", "sudo vmflow service install"} {
		if !strings.Contains(text, want) {
			t.Fatalf("service failure output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "init failed") || strings.Contains(text, "初始化失败") {
		t.Fatalf("service failure was mislabeled as init failure:\n%s", text)
	}
}

func TestPrintInitServiceFailureReportsRolledBackConfig(t *testing.T) {
	var output bytes.Buffer
	printInitServiceFailure(&output, initResult{ConfigPath: "/etc/vmflow/config.yaml"}, i18n.New(i18n.SimplifiedChinese), unsavedInitServiceError(errors.New("systemctl failed")))
	text := output.String()
	for _, want := range []string{"系统配置已回滚", "vmflow init --install-service"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rolled-back failure output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "配置已保存") || strings.Contains(text, "service install --config") {
		t.Fatalf("rolled-back failure claimed a saved config:\n%s", text)
	}
}

func TestInstallInitSystemServiceRollsBackNewConfigWhenServiceFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "config.yaml")
	wantErr := errors.New("service registration failed")
	err := installInitSystemServiceWithOps(initServicePayload{
		BinaryPath: "/usr/local/bin/vmflow", ConfigPath: configPath, ServiceName: "vmflow", Locale: i18n.English,
		Update: controlapi.SetupConfigUpdate{Language: i18n.English},
	}, io.Discard, initSystemInstallOps{
		elevated: true,
		plan:     installPlanFromConfig,
		install:  func(service.Config, io.Writer) error { return wantErr },
	})
	if !errors.Is(err, wantErr) || initServiceConfigSaved(err) {
		t.Fatalf("new-config failure = %v, saved=%t", err, initServiceConfigSaved(err))
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("new config was not rolled back: %v", statErr)
	}
}

func TestInstallInitSystemServiceKeepsExistingConfigWhenServiceFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("service registration failed")
	err := installInitSystemServiceWithOps(initServicePayload{
		BinaryPath: "/usr/local/bin/vmflow", ConfigPath: configPath, ServiceName: "vmflow", Locale: i18n.English,
		Update: controlapi.SetupConfigUpdate{Language: i18n.English},
	}, io.Discard, initSystemInstallOps{
		elevated: true,
		plan:     installPlanFromConfig,
		install:  func(service.Config, io.Writer) error { return wantErr },
	})
	if !errors.Is(err, wantErr) || !initServiceConfigSaved(err) {
		t.Fatalf("existing-config failure = %v, saved=%t", err, initServiceConfigSaved(err))
	}
	if _, statErr := os.Stat(configPath); statErr != nil {
		t.Fatalf("existing config was removed: %v", statErr)
	}
}

func TestDecodeInitServicePayloadRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"binary_path":"/bin/vmflow","unknown":true}`,
		`{"binary_path":"/bin/vmflow"} {}`,
	} {
		if _, err := decodeInitServicePayload(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid payload was accepted: %s", input)
		}
	}
}

func TestDecodeInitServicePayloadRejectsOversizedInput(t *testing.T) {
	input := `{"binary_path":"/bin/vmflow"}` + strings.Repeat(" ", initServiceHelperMaxPayload)
	if _, err := decodeInitServicePayload(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized payload error = %v", err)
	}
}

func TestInstallInitSystemServiceRejectsPayloadPlanMismatch(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	installed := false
	err := installInitSystemServiceWithOps(initServicePayload{
		BinaryPath: "/usr/local/bin/vmflow", ConfigPath: configPath, ServiceName: "vmflow", Locale: i18n.English,
		Update: controlapi.SetupConfigUpdate{Language: i18n.English},
	}, io.Discard, initSystemInstallOps{
		elevated: true,
		plan: func(service.Config) (service.InstallPlan, error) {
			return service.InstallPlan{
				BinaryPath:  "/usr/local/bin/vmflow",
				ConfigPath:  filepath.Join(filepath.Dir(configPath), "different.yaml"),
				ServiceName: "vmflow",
			}, nil
		},
		install: func(service.Config, io.Writer) error {
			installed = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("payload mismatch error = %v", err)
	}
	if installed {
		t.Fatal("service install ran for a mismatched payload")
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("mismatched payload wrote config: %v", statErr)
	}
}

func TestRestoreInitProfileAfterFailureOnlyRestoresUnsavedConfig(t *testing.T) {
	restores := 0
	result := initResult{restoreProfile: func() error {
		restores++
		return nil
	}}
	wantErr := errors.New("install failed")
	if err := restoreInitProfileAfterFailure(result, unsavedInitServiceError(wantErr)); !errors.Is(err, wantErr) {
		t.Fatalf("unsaved failure = %v", err)
	}
	if restores != 1 {
		t.Fatalf("unsaved restore calls = %d", restores)
	}
	if err := restoreInitProfileAfterFailure(result, savedInitServiceError(wantErr)); !errors.Is(err, wantErr) {
		t.Fatalf("saved failure = %v", err)
	}
	if restores != 1 {
		t.Fatalf("saved config unexpectedly restored profile; calls = %d", restores)
	}
}

func installPlanFromConfig(cfg service.Config) (service.InstallPlan, error) {
	return service.InstallPlan{
		BinaryPath:  cfg.BinaryPath,
		ConfigPath:  cfg.ConfigPath,
		ServiceName: cfg.ServiceName,
	}, nil
}
