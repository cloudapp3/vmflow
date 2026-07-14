//go:build windows

package service

import (
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestWindowsServiceCommandLineRoundTrip(t *testing.T) {
	cfg := Config{
		BinaryPath:  `C:\Program Files\vmflow\vmflow.exe`,
		ConfigPath:  `C:\ProgramData\vmflow config\config.yaml`,
		ServiceName: "vmflow custom",
		LogFile:     `C:\ProgramData\vmflow logs\vmflow.log`,
		ExtraArgs: []string{
			`-future-header=value with spaces and "quotes"`,
		},
		ControlListen:              "127.0.0.1:19090",
		InsecureAllowRemoteControl: true,
	}
	want := append([]string{cfg.BinaryPath}, windowsServiceArgs(cfg)...)
	commandLine := windows.ComposeCommandLine(want)
	got, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		t.Fatalf("decompose service command line: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command line round trip = %#v, want %#v\ncommand: %s", got, want, commandLine)
	}
}

func TestWindowsServiceArgsCarryRegisteredNameLast(t *testing.T) {
	cfg := Config{
		ConfigPath:  `C:\ProgramData\vmflow\config.yaml`,
		ServiceName: "edge relay",
		ExtraArgs:   []string{"-future-switch"},
	}
	args := windowsServiceArgs(cfg)
	if got := args[len(args)-2:]; !reflect.DeepEqual(got, []string{"-service-name", cfg.ServiceName}) {
		t.Fatalf("service name suffix = %#v, want registered name", got)
	}
}

func TestDesiredWindowsServiceConfigReplacesCommandAndEnablesBoot(t *testing.T) {
	current := mgr.Config{
		BinaryPathName:   `"C:\old\vmflow.exe" -config "C:\old.yaml"`,
		StartType:        mgr.StartDisabled,
		ServiceStartName: `NT SERVICE\vmflow-user`,
		Dependencies:     []string{"Tcpip"},
		SidType:          windows.SERVICE_SID_TYPE_RESTRICTED,
		DelayedAutoStart: true,
	}
	commandLine := `"C:\Program Files\vmflow\vmflow.exe" -config "C:\ProgramData\vmflow\config.yaml"`
	got := desiredWindowsServiceConfig(current, "vmflow-edge", commandLine)
	if got.BinaryPathName != commandLine {
		t.Fatalf("BinaryPathName = %q, want %q", got.BinaryPathName, commandLine)
	}
	if got.StartType != mgr.StartAutomatic || got.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Fatalf("startup config = start %d type %d, want automatic own-process", got.StartType, got.ServiceType)
	}
	if got.ErrorControl != mgr.ErrorNormal || got.DelayedAutoStart {
		t.Fatalf("error/delayed config = %d/%t, want normal/false", got.ErrorControl, got.DelayedAutoStart)
	}
	if got.ServiceStartName != "" || got.Password != "" {
		t.Fatalf("account fields must be omitted during update: %#v", got)
	}
	if !reflect.DeepEqual(got.Dependencies, current.Dependencies) || got.SidType != current.SidType {
		t.Fatalf("dependencies/SID were not preserved: %#v", got)
	}
}

func TestDesiredWindowsRecoveryPolicy(t *testing.T) {
	got := desiredWindowsRecoveryPolicy()
	if got.ResetPeriod != 86400 {
		t.Fatalf("ResetPeriod = %d, want 86400", got.ResetPeriod)
	}
	if !got.NonCrashFailures {
		t.Fatal("non-crash recovery must be enabled")
	}
	wantActions := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}
	if !reflect.DeepEqual(got.Actions, wantActions) {
		t.Fatalf("Actions = %#v, want %#v", got.Actions, wantActions)
	}
}
