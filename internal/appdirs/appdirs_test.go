package appdirs

import (
	"errors"
	"testing"
)

func TestConfigPathForSupportedPlatforms(t *testing.T) {
	env := map[string]string{
		"XDG_CONFIG_HOME": "/srv/alice/config",
		"ProgramData":     `D:\ProgramData`,
		"LocalAppData":    `D:\Users\alice\AppData\Local`,
	}
	getenv := func(name string) string { return env[name] }
	home := func() (string, error) { return "/Users/alice", nil }

	tests := []struct {
		name  string
		goos  string
		scope Scope
		want  string
	}{
		{name: "linux system", goos: "linux", scope: System, want: "/etc/vmflow/config.yaml"},
		{name: "linux user xdg", goos: "linux", scope: User, want: "/srv/alice/config/vmflow/config.yaml"},
		{name: "darwin system", goos: "darwin", scope: System, want: "/Library/Application Support/vmflow/config.yaml"},
		{name: "darwin user", goos: "darwin", scope: User, want: "/Users/alice/Library/Application Support/vmflow/config.yaml"},
		{name: "windows system", goos: "windows", scope: System, want: `D:\ProgramData\vmflow\config.yaml`},
		{name: "windows user", goos: "windows", scope: User, want: `D:\Users\alice\AppData\Local\vmflow\config.yaml`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configPathFor(tc.goos, tc.scope, getenv, home)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("config path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigPathForUsesPlatformFallbacks(t *testing.T) {
	emptyEnv := func(string) string { return "" }

	linux, err := configPathFor("linux", User, emptyEnv, func() (string, error) { return "/home/alice", nil })
	if err != nil || linux != "/home/alice/.config/vmflow/config.yaml" {
		t.Fatalf("linux fallback = %q, %v", linux, err)
	}
	windowsSystem, err := configPathFor("windows", System, emptyEnv, nil)
	if err != nil || windowsSystem != `C:\ProgramData\vmflow\config.yaml` {
		t.Fatalf("windows system fallback = %q, %v", windowsSystem, err)
	}
	windowsUser, err := configPathFor("windows", User, emptyEnv, func() (string, error) { return `C:\Users\alice`, nil })
	if err != nil || windowsUser != `C:\Users\alice\AppData\Local\vmflow\config.yaml` {
		t.Fatalf("windows user fallback = %q, %v", windowsUser, err)
	}
}

func TestConfigPathForIgnoresRelativeXDGAndRejectsBadWindowsRoots(t *testing.T) {
	linux, err := configPathFor(
		"linux",
		User,
		func(string) string { return "relative" },
		func() (string, error) { return "/home/alice", nil },
	)
	if err != nil || linux != "/home/alice/.config/vmflow/config.yaml" {
		t.Fatalf("relative XDG fallback = %q, %v", linux, err)
	}

	for _, scope := range []Scope{User, System} {
		_, err := configPathFor("windows", scope, func(string) string { return "relative" }, nil)
		if err == nil {
			t.Fatalf("windows scope %d accepted a relative root", scope)
		}
	}
}

func TestConfigPathForReportsUnsupportedOSAndMissingHome(t *testing.T) {
	if _, err := configPathFor("plan9", User, nil, nil); err == nil {
		t.Fatal("unsupported OS did not fail")
	}
	if _, err := configPathFor("darwin", User, nil, func() (string, error) { return "", errors.New("missing") }); err == nil {
		t.Fatal("missing home did not fail")
	}
}
