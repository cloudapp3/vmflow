package service

import (
	"fmt"
	"strings"
)

// daemonArgs builds the daemon command-line arguments (after the binary path)
// shared across platforms. logFileFlag controls whether -log-file is appended
// (macOS uses launchd capture paths instead and omits it).
func daemonArgs(cfg Config, logFileFlag bool) []string {
	args := []string{"daemon", "-config", cfg.ConfigPath}
	if logFileFlag {
		if lp := strings.TrimSpace(cfg.LogFile); lp != "" {
			args = append(args, "-log-file", lp)
		}
	}
	if ea := strings.TrimSpace(cfg.ExtraArgs); ea != "" {
		args = append(args, ea)
	}
	return args
}

// systemdExecStart renders the ExecStart= value for the systemd unit. Every
// token is double-quoted so paths containing spaces survive systemd's parser.
func systemdExecStart(cfg Config) string {
	tokens := []string{shellQuote(cfg.BinaryPath)}
	for _, a := range daemonArgs(cfg, true) {
		tokens = append(tokens, shellQuote(a))
	}
	return strings.Join(tokens, " ")
}

// systemdUnit renders the full systemd unit. It runs the daemon as root by
// default (simplest for forwarding privileged ports) with CAP_NET_BIND_SERVICE
// in its ambient set and auto-restart on failure.
func systemdUnit(cfg Config) string {
	userLine := ""
	if u := strings.TrimSpace(cfg.User); u != "" {
		userLine = fmt.Sprintf("User=%s\nGroup=%s\n", u, u)
	}
	return fmt.Sprintf(`[Unit]
Description=vmflow L4 forwarding daemon
Documentation=https://github.com/cloudapp3/vmflow
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
%s[Install]
WantedBy=multi-user.target
`, systemdExecStart(cfg), userLine)
}

// launchdLabel returns the reverse-DNS label for the launchd daemon.
func launchdLabel(cfg Config) string {
	name := strings.TrimSpace(cfg.ServiceName)
	if name == "" {
		name = DefaultServiceName
	}
	// keep the label a valid single path component
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == ':' || r == ' ' {
			return '-'
		}
		return r
	}, strings.ToLower(name))
	return "io.cloudapp." + name
}

// plistProgramArguments renders the <string> entries for ProgramArguments. plist
// arrays are already tokenized, so values are NOT quoted.
func plistProgramArguments(cfg Config) string {
	var b strings.Builder
	args := append([]string{cfg.BinaryPath}, daemonArgs(cfg, false)...)
	for _, a := range args {
		b.WriteString("    <string>")
		// escape XML special chars
		s := strings.ReplaceAll(a, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		s = strings.ReplaceAll(s, ">", "&gt;")
		b.WriteString(s)
		b.WriteString("</string>\n")
	}
	return b.String()
}

// launchdLogPaths returns (stdout, stderr) capture paths. If cfg.LogFile is set
// it is used for stdout and a sibling .err for stderr; otherwise defaults under
// /var/log/vmflow.
func launchdLogPaths(cfg Config) (string, string) {
	if lp := strings.TrimSpace(cfg.LogFile); lp != "" {
		return lp, lp + ".err"
	}
	return "/var/log/vmflow/vmflow.out.log", "/var/log/vmflow/vmflow.err.log"
}

// launchdPlist renders the launchd daemon plist (system domain, runs at boot).
func launchdPlist(cfg Config) string {
	stdout, stderr := launchdLogPaths(cfg)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, launchdLabel(cfg), plistProgramArguments(cfg), stdout, stderr)
}

// scBinPath renders the binPath= value for `sc create` on Windows. sc.exe has a
// peculiar parser: the entire command line is wrapped in outer quotes and inner
// exe/config paths are escaped with backslash-quotes. This is the documented
// form that survives spaces in "Program Files".
func scBinPath(cfg Config) string {
	cmd := fmt.Sprintf("\\\"%s\\\" daemon -config \\\"%s\\\"", cfg.BinaryPath, cfg.ConfigPath)
	if lp := strings.TrimSpace(cfg.LogFile); lp != "" {
		cmd += fmt.Sprintf(" -log-file \\\"%s\\\"", lp)
	}
	if ea := strings.TrimSpace(cfg.ExtraArgs); ea != "" {
		cmd += " " + ea
	}
	return "\"" + cmd + "\""
}
