package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeMCPControlAddress(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "ipv4", input: " http://127.0.0.1:19090/ ", want: "http://127.0.0.1:19090"},
		{name: "localhost tls", input: "HTTPS://localhost:19090", want: "https://localhost:19090"},
		{name: "ipv6", input: "http://[::1]:19090", want: "http://[::1]:19090"},
		{name: "remote host", input: "https://control.example.com", wantErr: true},
		{name: "credentials", input: "http://token@127.0.0.1:19090", wantErr: true},
		{name: "path", input: "http://127.0.0.1:19090/v1", wantErr: true},
		{name: "query", input: "http://127.0.0.1:19090?token=x", wantErr: true},
		{name: "wrong scheme", input: "file:///tmp/vmflow.sock", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeMCPControlAddress(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeMCPControlAddress(%q) = %q, want error", tc.input, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalizeMCPControlAddress(%q) = (%q, %v), want (%q, nil)", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestParseMCPOptionsUsesEnvironmentAndRejectsUnsafeTLS(t *testing.T) {
	t.Setenv("VMFLOW_CONTROL_TOKEN", " viewer-secret ")
	t.Setenv("VMFLOW_HEADERS", "X-Operator: mcp")
	var output bytes.Buffer
	opts, err := parseMCPOptions([]string{"-addr", "http://localhost:19091"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if opts.addr != "http://localhost:19091" || opts.token != "viewer-secret" {
		t.Fatalf("unexpected MCP options: %+v", opts)
	}
	if got := opts.headers.HTTPHeader().Get("X-Operator"); got != "mcp" {
		t.Fatalf("MCP header = %q, want mcp", got)
	}

	_, err = parseMCPOptions([]string{"-addr", "http://127.0.0.1:19090", "-tls-skip-verify"}, &output)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unsafe TLS options error = %v, want https validation", err)
	}
	if _, err := parseMCPOptions([]string{"unexpected"}, &output); err == nil {
		t.Fatal("parseMCPOptions accepted a positional argument")
	}
}

func TestRunMCPWithIOStopsCleanlyOnEOF(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runMCPWithIO(context.Background(), nil, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("MCP EOF output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestParseMCPOptionsHelp(t *testing.T) {
	var output bytes.Buffer
	_, err := parseMCPOptions([]string{"--help"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(output.String(), "vmflow mcp") || !strings.Contains(output.String(), "read-only") {
		t.Fatalf("unexpected MCP help: %q", output.String())
	}
}

func TestMCPCommandTransport(t *testing.T) {
	const helperEnv = "VMFLOW_TEST_MCP_STDIO_HELPER"
	if os.Getenv(helperEnv) == "1" {
		if err := runMCPWithIO(context.Background(), nil, os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPCommandTransport$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	var childStderr bytes.Buffer
	cmd.Stderr = &childStderr

	client := mcp.NewClient(&mcp.Implementation{Name: "vmflow-command-test", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("connect to vmflow mcp: %v; stderr=%s", err, childStderr.String())
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		t.Fatalf("list MCP tools: %v; stderr=%s", err, childStderr.String())
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{
		"get_forwarding_rule",
		"get_traffic_stats",
		"get_vmflow_status",
		"list_forwarding_rules",
		"run_config_precheck",
	}
	if !slices.Equal(names, want) {
		_ = session.Close()
		t.Fatalf("MCP tools = %v, want %v", names, want)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close MCP command session: %v; stderr=%s", err, childStderr.String())
	}
}
