package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallLaunchdDaemonReplacesLoadedDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	if err := os.WriteFile(path, []byte("old plist\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var commands []string
	runner := func(argv []string) ([]byte, error) {
		commands = append(commands, strings.Join(argv, " "))
		if len(argv) > 1 && argv[1] == "print" {
			return []byte("path = " + path + "\n"), nil
		}
		return nil, nil
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installLaunchdDaemon(cfg, path, runner); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<key>ProgramArguments</key>") {
		t.Fatalf("new plist was not installed:\n%s", data)
	}
	joined := "\n" + strings.Join(commands, "\n") + "\n"
	if !strings.Contains(joined, "\nlaunchctl bootout system/io.cloudapp.vmflow\n") ||
		!strings.HasSuffix(joined, "launchctl bootstrap system "+path+"\n") {
		t.Fatalf("loaded daemon was not replaced: %v", commands)
	}
}

func TestStartLaunchdDaemonBootstrapsUnloadedService(t *testing.T) {
	path := "/Library/LaunchDaemons/io.cloudapp.vmflow.plist"
	var commands []string
	printCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if strings.Contains(command, "launchctl print") {
			printCalls++
			if printCalls == 1 {
				return []byte("Could not find service"), errors.New("exit status 113")
			}
			return []byte("state = running\npath = " + path + "\n"), nil
		}
		return nil, nil
	}
	if err := startLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl print system/io.cloudapp.vmflow",
		"launchctl bootstrap system " + path,
		"launchctl print system/io.cloudapp.vmflow",
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("start commands = %v, want %v", commands, want)
	}
}

func TestStartLaunchdDaemonKickstartsLoadedService(t *testing.T) {
	path := "/Library/LaunchDaemons/io.cloudapp.vmflow.plist"
	var commands []string
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if strings.Contains(command, "launchctl print") {
			return []byte("state = running\npath = " + path + "\n"), nil
		}
		return nil, nil
	}
	if err := startLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl print system/io.cloudapp.vmflow",
		"launchctl kickstart -k system/io.cloudapp.vmflow",
		"launchctl print system/io.cloudapp.vmflow",
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("start commands = %v, want %v", commands, want)
	}
}

func TestUninstallLaunchdDaemonBootsOutAndVerifiesBeforeRemovingPlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.cloudapp.vmflow.plist")
	if err := os.WriteFile(path, []byte("plist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	printCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if strings.Contains(command, "launchctl print") {
			printCalls++
			if printCalls == 1 {
				return []byte("state = running\npath = " + path + "\n"), nil
			}
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		return nil, nil
	}

	if err := uninstallLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl print system/io.cloudapp.vmflow",
		"launchctl bootout system/io.cloudapp.vmflow",
		"launchctl print system/io.cloudapp.vmflow",
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("uninstall commands = %v, want %v", commands, want)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("launchd plist was not removed: %v", err)
	}
}

func TestUninstallLaunchdDaemonAllowsAlreadyUnloadedService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.cloudapp.vmflow.plist")
	if err := os.WriteFile(path, []byte("plist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	runner := func(argv []string) ([]byte, error) {
		commands = append(commands, strings.Join(argv, " "))
		return []byte("Could not find service"), errors.New("exit status 113")
	}

	if err := uninstallLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner); err != nil {
		t.Fatal(err)
	}
	want := []string{"launchctl print system/io.cloudapp.vmflow"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("unloaded uninstall commands = %v, want %v", commands, want)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unloaded launchd plist was not removed: %v", err)
	}
}

func TestUninstallLaunchdDaemonPreservesPlistWhenBootoutFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.cloudapp.vmflow.plist")
	if err := os.WriteFile(path, []byte("plist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if strings.Contains(command, "launchctl print") {
			return []byte("state = running\npath = " + path + "\n"), nil
		}
		return []byte("operation not permitted"), errors.New("exit status 1")
	}

	err := uninstallLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootout") {
		t.Fatalf("bootout error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("launchd plist changed after failed bootout: %v", err)
	}
	want := []string{
		"launchctl print system/io.cloudapp.vmflow",
		"launchctl bootout system/io.cloudapp.vmflow",
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands after failed bootout = %v, want %v", commands, want)
	}
}

func TestUninstallLaunchdDaemonPreservesPlistWhenDaemonRemainsLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.cloudapp.vmflow.plist")
	if err := os.WriteFile(path, []byte("plist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		commands = append(commands, command)
		if strings.Contains(command, "launchctl print") {
			return []byte("state = running\npath = " + path + "\n"), nil
		}
		return nil, nil
	}

	err := uninstallLaunchdDaemon(Config{ServiceName: "vmflow"}, path, runner)
	if err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("loaded verification error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("launchd plist changed while daemon remained loaded: %v", err)
	}
	want := []string{
		"launchctl print system/io.cloudapp.vmflow",
		"launchctl bootout system/io.cloudapp.vmflow",
		"launchctl print system/io.cloudapp.vmflow",
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands after loaded verification = %v, want %v", commands, want)
	}
}

func TestInstallLaunchdDaemonRestoresPreviousDefinitionOnBootstrapFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	oldPlist := []byte("old plist\n")
	if err := os.WriteFile(path, oldPlist, 0o640); err != nil {
		t.Fatal(err)
	}

	printCalls := 0
	bootstrapCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		switch {
		case strings.Contains(command, "launchctl print"):
			printCalls++
			if printCalls == 1 {
				return []byte("path = " + path + "\n"), nil
			}
			return []byte("Could not find service"), errors.New("exit status 113")
		case strings.Contains(command, "launchctl bootstrap"):
			bootstrapCalls++
			if bootstrapCalls == 1 {
				return []byte("new daemon failed"), errors.New("exit status 5")
			}
		}
		return nil, nil
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	err := installLaunchdDaemon(cfg, path, runner)
	if err == nil || !strings.Contains(err.Error(), "previous plist and launchd state restored") {
		t.Fatalf("expected restored install error, got %v", err)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(oldPlist) {
		t.Fatalf("old plist was not restored: %q", data)
	}
	if bootstrapCalls != 2 {
		t.Fatalf("expected failed new bootstrap and restored old bootstrap, got %d", bootstrapCalls)
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("old plist mode was not restored: info=%v err=%v", info, statErr)
	}
}

func TestInstallLaunchdDaemonRemovesFirstInstallOnBootstrapFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	bootstrapCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		switch {
		case strings.Contains(command, "launchctl print"):
			return []byte("Could not find service"), errors.New("exit status 113")
		case strings.Contains(command, "launchctl bootstrap"):
			bootstrapCalls++
			return []byte("invalid plist"), errors.New("exit status 5")
		default:
			return nil, nil
		}
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installLaunchdDaemon(cfg, path, runner); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if bootstrapCalls != 1 {
		t.Fatalf("unexpected bootstrap count: %d", bootstrapCalls)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("new plist was left behind: %v", err)
	}
}

func TestInstallLaunchdDaemonAbortsWhenLoadedStateIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	oldPlist := []byte("old plist\n")
	if err := os.WriteFile(path, oldPlist, 0o640); err != nil {
		t.Fatal(err)
	}

	commands := 0
	runner := func(argv []string) ([]byte, error) {
		commands++
		return []byte("Operation not permitted"), errors.New("exit status 1")
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installLaunchdDaemon(cfg, path, runner); err == nil || !strings.Contains(err.Error(), "inspect existing launchd daemon") {
		t.Fatalf("expected state-probe error, got %v", err)
	}
	if commands != 1 {
		t.Fatalf("unexpected launchctl mutations: %d commands", commands)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(oldPlist) {
		t.Fatalf("plist changed after failed state probe: %q", data)
	}
}

func TestInstallLaunchdDaemonDoesNotRepeatFailedInitialBootout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	oldPlist := []byte("old plist\n")
	if err := os.WriteFile(path, oldPlist, 0o640); err != nil {
		t.Fatal(err)
	}

	bootoutCalls := 0
	bootstrapCalls := 0
	runner := func(argv []string) ([]byte, error) {
		command := strings.Join(argv, " ")
		switch {
		case strings.Contains(command, "launchctl print"):
			return []byte("path = " + path + "\n"), nil
		case strings.Contains(command, "launchctl bootout"):
			bootoutCalls++
			return []byte("temporary launchd error"), errors.New("exit status 5")
		case strings.Contains(command, "launchctl bootstrap"):
			bootstrapCalls++
		}
		return nil, nil
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	err := installLaunchdDaemon(cfg, path, runner)
	if err == nil || !strings.Contains(err.Error(), "previous plist and launchd state restored") {
		t.Fatalf("expected restored bootout error, got %v", err)
	}
	if bootoutCalls != 1 || bootstrapCalls != 0 {
		t.Fatalf("rollback disturbed the loaded old daemon: bootout=%d bootstrap=%d", bootoutCalls, bootstrapCalls)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(oldPlist) {
		t.Fatalf("old plist was not restored: %q", data)
	}
}

func TestInstallLaunchdDaemonRejectsLoadedJobWithoutRestorablePlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	commands := 0
	runner := func(argv []string) ([]byte, error) {
		commands++
		return []byte("path = " + path + "\n"), nil
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installLaunchdDaemon(cfg, path, runner); err == nil || !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Fatalf("expected non-restorable loaded-job error, got %v", err)
	}
	if commands != 1 {
		t.Fatalf("unexpected launchctl mutations: %d commands", commands)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("plist was unexpectedly created: %v", err)
	}
}

func TestInstallLaunchdDaemonRejectsLoadedJobFromDifferentPlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmflow.plist")
	oldPlist := []byte("old plist\n")
	if err := os.WriteFile(path, oldPlist, 0o640); err != nil {
		t.Fatal(err)
	}

	commands := 0
	runner := func(argv []string) ([]byte, error) {
		commands++
		return []byte("path = /Library/LaunchDaemons/other.plist\n"), nil
	}
	cfg := Config{BinaryPath: "/usr/local/bin/vmflow", ConfigPath: "/Library/Application Support/vmflow/config.yaml", ServiceName: "vmflow"}
	if err := installLaunchdDaemon(cfg, path, runner); err == nil || !strings.Contains(err.Error(), "cannot be rolled back safely") {
		t.Fatalf("expected different-source rejection, got %v", err)
	}
	if commands != 1 {
		t.Fatalf("unexpected launchctl mutations: %d commands", commands)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(oldPlist) {
		t.Fatalf("plist changed for a differently sourced job: %q", data)
	}
}
