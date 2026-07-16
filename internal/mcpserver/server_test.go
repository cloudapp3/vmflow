package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerAdvertisesOnlyReadOnlyTools(t *testing.T) {
	session := connectClient(t, NewServer(newFakeManagementClient(), "v1.2.3"))
	capabilities := session.InitializeResult().Capabilities
	if capabilities.Tools == nil || capabilities.Logging != nil || capabilities.Prompts != nil || capabilities.Resources != nil {
		t.Fatalf("unexpected server capabilities: %+v", capabilities)
	}

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools returned error: %v", err)
	}
	gotNames := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		gotNames = append(gotNames, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not marked read-only", tool.Name)
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil {
			t.Errorf("tool %q has no explicit open-world annotation", tool.Name)
		} else {
			wantOpenWorld := tool.Name == toolRunConfigPrecheck
			if *tool.Annotations.OpenWorldHint != wantOpenWorld {
				t.Errorf("tool %q open-world = %v, want %v", tool.Name, *tool.Annotations.OpenWorldHint, wantOpenWorld)
			}
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %q is missing an input or output schema", tool.Name)
		}
	}
	slices.Sort(gotNames)
	wantNames := []string{
		toolGetForwardingRule,
		toolGetTrafficStats,
		toolGetVMFlowStatus,
		toolListForwardingRules,
		toolRunConfigPrecheck,
	}
	slices.Sort(wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("tool names = %v, want %v", gotNames, wantNames)
	}
}

func TestServerReturnsDisconnectedStatusAsStructuredSuccess(t *testing.T) {
	backend := newFakeManagementClient()
	backend.sessionErr = errors.New("connection refused")
	session := connectClient(t, NewServer(backend, "vtest"))

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolGetVMFlowStatus})
	if err != nil {
		t.Fatalf("CallTool returned protocol error: %v", err)
	}
	if result.IsError {
		t.Fatalf("disconnected status returned tool error: %+v", result.Content)
	}
	var got getVMFlowStatusOutput
	decodeStructured(t, result.StructuredContent, &got)
	if got.Connected || got.MCPServerVersion != "vtest" {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Issues) != 1 || !strings.Contains(got.Issues[0], "connection refused") {
		t.Fatalf("status issues = %v", got.Issues)
	}
	if backend.configCalls != 0 || backend.rulesCalls != 0 || backend.statsCalls != 0 {
		t.Fatalf("offline status made follow-up calls: config=%d rules=%d stats=%d", backend.configCalls, backend.rulesCalls, backend.statsCalls)
	}
}

func TestServerReturnsToolValidationErrors(t *testing.T) {
	session := connectClient(t, NewServer(newFakeManagementClient(), "vtest"))
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      toolListForwardingRules,
		Arguments: map[string]any{"protocol": "quic"},
	})
	if err != nil {
		t.Fatalf("CallTool returned protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("invalid protocol did not produce a tool error")
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "protocol") {
		t.Fatalf("unexpected tool error content: %+v", result.Content)
	}
}

func TestRunStdioValidatesStreamsAndKeepsEOFQuiet(t *testing.T) {
	backend := newFakeManagementClient()
	if err := RunStdio(context.Background(), nil, &bytes.Buffer{}, backend, "vtest"); err == nil {
		t.Fatal("RunStdio accepted nil stdin")
	}
	if err := RunStdio(context.Background(), strings.NewReader(""), nil, backend, "vtest"); err == nil {
		t.Fatal("RunStdio accepted nil stdout")
	}
	var stdout bytes.Buffer
	if err := RunStdio(context.Background(), strings.NewReader(""), &stdout, backend, "vtest"); err != nil {
		t.Fatalf("RunStdio on EOF returned error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("RunStdio wrote unexpected output: %q", stdout.String())
	}
}

func connectClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect returned error: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "vmflow-test", Version: "v1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect returned error: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func decodeStructured(t *testing.T, value any, dst any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}
