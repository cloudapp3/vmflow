package main

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/tui"
)

func TestLocalServiceControlEligibility(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		explicit    bool
		environment map[string]string
		want        bool
	}{
		{name: "default IPv4", address: "http://127.0.0.1:19090", want: true},
		{name: "localhost", address: "https://localhost:19090", want: true},
		{name: "IPv6 loopback", address: "http://[::1]:19090", want: true},
		{name: "explicit loopback may be a tunnel", address: "http://127.0.0.1:19090", explicit: true},
		{name: "address environment may be a tunnel", address: "http://127.0.0.1:19090", environment: map[string]string{"VMFLOW_CONTROL_ADDR": "http://127.0.0.1:19090"}},
		{name: "custom profile", address: "http://127.0.0.1:19090", environment: map[string]string{"VMFLOW_CLIENT_CONFIG": "/tmp/client.yaml"}},
		{name: "remote address", address: "http://192.0.2.1:19090"},
		{name: "management path", address: "http://127.0.0.1:19090/proxy"},
		{name: "credentials", address: "http://user@example.com:19090"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(key string) string { return test.environment[key] }
			if got := localServiceControlEligible(test.address, test.explicit, getenv); got != test.want {
				t.Fatalf("eligibility = %t, want %t", got, test.want)
			}
		})
	}
}

func TestNativeServiceInstallCommandQuotesConfigPath(t *testing.T) {
	command := nativeServiceInstallCommand("/srv/vm flow/config's.yaml")
	if !strings.Contains(command, "vmflow service install --config") || !strings.Contains(command, "config") {
		t.Fatalf("install command = %q", command)
	}
	if runtime.GOOS != "windows" && !strings.HasPrefix(command, "sudo ") {
		t.Fatalf("Unix install command = %q, want sudo prefix", command)
	}
}

func TestLocalTUIServiceReadyRequiresInstalledRunningService(t *testing.T) {
	tests := []struct {
		name       string
		summary    tui.ServiceSummary
		inspectErr error
		wantReady  bool
		wantOutput []string
	}{
		{
			name:       "not installed",
			summary:    tui.ServiceSummary{State: "not installed"},
			wantOutput: []string{"尚未安装", "sudo vmflow service install --config '/etc/vmflow/config.yaml'", "vmflow tui"},
		},
		{
			name:       "stopped",
			summary:    tui.ServiceSummary{Installed: true, State: "inactive"},
			wantOutput: []string{"当前已停止", "vmflow service start", "vmflow tui"},
		},
		{
			name:      "running",
			summary:   tui.ServiceSummary{Installed: true, Running: true, State: "active"},
			wantReady: true,
		},
		{
			name:       "inspection inconclusive",
			inspectErr: errors.New("service manager unavailable"),
			wantReady:  true,
		},
		{
			name:      "unknown state",
			summary:   tui.ServiceSummary{State: "unknown", Detail: "unit path is not a regular file"},
			wantReady: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &tui.ServiceController{
				Inspect: func(context.Context) (tui.ServiceSummary, error) {
					return test.summary, test.inspectErr
				},
				InstallCommand: "sudo vmflow service install --config '/etc/vmflow/config.yaml'",
			}
			var output bytes.Buffer
			ready := localTUIServiceReady(t.Context(), &output, i18n.New(i18n.SimplifiedChinese), controller)
			if ready != test.wantReady {
				t.Fatalf("ready = %t, want %t; output=%q", ready, test.wantReady, output.String())
			}
			for _, want := range test.wantOutput {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("output missing %q: %q", want, output.String())
				}
			}
			if test.wantReady && output.Len() != 0 {
				t.Fatalf("ready preflight wrote output: %q", output.String())
			}
		})
	}
}
