package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newWriteToolHandlers(client *fakeManagementClient) *toolHandlers {
	return newToolHandlersForBackend(newManagementBackend(client), managementWriteBackend{write: client}, "vtest")
}

func writeTestConfig(rules ...engine.Rule) *controlapi.ConfigRulesResponse {
	return &controlapi.ConfigRulesResponse{
		Revision:       "rev-1",
		Writable:       true,
		UDPMaxSessions: 256,
		Rules:          rules,
		ETag:           `"rev-1"`,
	}
}

func baseUpsertInput() upsertForwardingRuleInput {
	return upsertForwardingRuleInput{
		RuleID:     "web",
		Name:       "web forward",
		Protocol:   "tcp",
		ListenPort: 8080,
		TargetAddr: "203.0.113.10",
		TargetPort: 80,
	}
}

func TestUpsertCreatesRulePreservingOthers(t *testing.T) {
	client := newFakeManagementClient()
	existing := engine.Rule{RuleID: "ssh", Name: "ssh", Protocol: engine.ProtocolTCP, ListenAddr: "127.0.0.1", ListenPort: 2222, TargetAddr: "127.0.0.1", TargetPort: 22, Enabled: true, CreatedTime: 111}
	client.config = writeTestConfig(existing)

	_, got, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, baseUpsertInput())
	if err != nil {
		t.Fatalf("upsertForwardingRule returned error: %v", err)
	}
	if got.Action != "created" || !got.Applied || got.DryRun {
		t.Fatalf("unexpected action output: %+v", got)
	}
	if client.applyCalls != 1 || client.precheckDoCalls != 0 {
		t.Fatalf("apply calls = %d, precheck calls = %d", client.applyCalls, client.precheckDoCalls)
	}
	if client.applyMatch != `"rev-1"` {
		t.Fatalf("apply If-Match = %q, want the snapshot ETag", client.applyMatch)
	}
	draft := client.applySeen[0]
	if len(draft.Rules) != 2 || draft.UDPMaxSessions != 256 {
		t.Fatalf("draft must keep the other rule and udp_max_sessions: %+v", draft)
	}
	if draft.Rules[0].RuleID != "ssh" || draft.Rules[0].CreatedTime != 111 {
		t.Fatalf("existing rule was modified: %+v", draft.Rules[0])
	}
	if draft.Rules[1].RuleID != "web" || draft.Rules[1].ListenAddr != "127.0.0.1" || !draft.Rules[1].Enabled {
		t.Fatalf("new rule was not appended as configured: %+v", draft.Rules[1])
	}
	if got.Rule == nil || got.Rule.RuleID != "web" {
		t.Fatalf("output rule = %+v", got.Rule)
	}
}

func TestUpsertUpdatesRulePreservingIdentity(t *testing.T) {
	client := newFakeManagementClient()
	existing := engine.Rule{RuleID: "web", Name: "old name", Protocol: engine.ProtocolTCP, ListenAddr: "127.0.0.1", ListenPort: 8080, TargetAddr: "192.0.2.1", TargetPort: 80, Enabled: false, CreatedTime: 111, Revision: 7}
	client.config = writeTestConfig(existing)

	input := baseUpsertInput()
	input.Name = "new name"
	input.ListenPort = 9090
	_, got, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("upsertForwardingRule returned error: %v", err)
	}
	if got.Action != "updated" || !got.Applied {
		t.Fatalf("unexpected action output: %+v", got)
	}
	draft := client.applySeen[0]
	if len(draft.Rules) != 1 {
		t.Fatalf("draft rules = %d, want 1", len(draft.Rules))
	}
	updated := draft.Rules[0]
	if updated.Name != "new name" || updated.ListenPort != 9090 || !updated.Enabled {
		t.Fatalf("rule fields were not replaced: %+v", updated)
	}
	if updated.CreatedTime != 111 || updated.Revision != 7 {
		t.Fatalf("identity fields were not preserved: %+v", updated)
	}
}

func TestUpsertDryRunPrechecksWithoutApplying(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	client.precheckDraft = &controlapi.PrecheckResponse{
		Revision: "rev-1",
		Diff:     []controlapi.ConfigRuleDiff{{RuleID: "web", ConfigAction: "add"}},
		Precheck: precheck.Result{OK: false, ErrorCount: 1, WarningCount: 2, Items: []precheck.Item{{Severity: precheck.SeverityError, Check: "bind", RuleID: "web", Message: "port in use"}}},
	}

	input := baseUpsertInput()
	input.DryRun = true
	_, got, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("dry-run upsert returned error: %v", err)
	}
	if got.Applied || !got.DryRun {
		t.Fatalf("dry-run output must not be applied: %+v", got)
	}
	if client.applyCalls != 0 || client.precheckDoCalls != 1 {
		t.Fatalf("apply calls = %d, precheck calls = %d", client.applyCalls, client.precheckDoCalls)
	}
	if got.Precheck == nil || got.Precheck.OK || got.Precheck.ErrorCount != 1 {
		t.Fatalf("precheck findings missing: %+v", got.Precheck)
	}
	if len(got.Diff) != 1 || got.Diff[0].RuleID != "web" {
		t.Fatalf("diff missing: %+v", got.Diff)
	}
	if len(got.Issues) != 1 || !strings.Contains(got.Issues[0], "1 error") {
		t.Fatalf("issues = %v", got.Issues)
	}
}

func TestUpsertParsesQuantityPolicies(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	enabled := false
	input := baseUpsertInput()
	input.Enabled = &enabled
	input.Bandwidth = &bandwidthLimitInput{Rate: "100 Mbps"}
	input.TrafficQuota = &trafficQuotaInput{Daily: "50GB", Timezone: "Asia/Shanghai"}

	_, got, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("upsertForwardingRule returned error: %v", err)
	}
	rule := client.applySeen[0].Rules[0]
	if rule.Enabled {
		t.Fatalf("enabled override was ignored: %+v", rule)
	}
	if rule.Bandwidth == nil || rule.Bandwidth.Rate != engine.ByteRate(12_500_000) {
		t.Fatalf("bandwidth rate not parsed: %+v", rule.Bandwidth)
	}
	if rule.TrafficQuota == nil || rule.TrafficQuota.Daily != engine.ByteSize(50_000_000_000) || rule.TrafficQuota.Timezone != "Asia/Shanghai" {
		t.Fatalf("traffic quota not parsed: %+v", rule.TrafficQuota)
	}
	if got.Rule == nil || got.Rule.Enabled {
		t.Fatalf("output rule = %+v", got.Rule)
	}
}

func TestUpsertRejectsInvalidInput(t *testing.T) {
	handlers := newWriteToolHandlers(newFakeManagementClient())
	cases := []struct {
		name string
		mut  func(*upsertForwardingRuleInput)
		want string
	}{
		{"missing rule id", func(in *upsertForwardingRuleInput) { in.RuleID = " " }, "rule_id"},
		{"missing name", func(in *upsertForwardingRuleInput) { in.Name = "" }, "name"},
		{"disabled protocol", func(in *upsertForwardingRuleInput) { in.Protocol = "http" }, "protocol"},
		{"unknown protocol", func(in *upsertForwardingRuleInput) { in.Protocol = "sctp" }, "protocol"},
		{"listen port zero", func(in *upsertForwardingRuleInput) { in.ListenPort = 0 }, "listen_port"},
		{"listen port high", func(in *upsertForwardingRuleInput) { in.ListenPort = 70000 }, "listen_port"},
		{"missing target", func(in *upsertForwardingRuleInput) { in.TargetAddr = "" }, "target_addr"},
		{"target port high", func(in *upsertForwardingRuleInput) { in.TargetPort = 65536 }, "target_port"},
		{"bad bandwidth unit", func(in *upsertForwardingRuleInput) { in.Bandwidth = &bandwidthLimitInput{Rate: "10 flops"} }, "bandwidth_limit.rate"},
		{"non-loopback default", func(in *upsertForwardingRuleInput) { in.ListenAddr = "0.0.0.0" }, "allow_non_loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := baseUpsertInput()
			tc.mut(&input)
			_, _, err := handlers.upsertForwardingRule(context.Background(), nil, input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestUpsertNonLoopbackRequiresExplicitConfirmation(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	input := baseUpsertInput()
	input.ListenAddr = "0.0.0.0"
	input.AllowNonLoopback = true

	_, got, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("upsertForwardingRule returned error: %v", err)
	}
	if !got.Applied {
		t.Fatalf("confirmed non-loopback rule was not applied: %+v", got)
	}
	if rule := client.applySeen[0].Rules[0]; rule.ListenAddr != "0.0.0.0" {
		t.Fatalf("listen address = %q, want 0.0.0.0", rule.ListenAddr)
	}
}

func TestDeleteRule(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig(
		engine.Rule{RuleID: "ssh", Name: "ssh"},
		engine.Rule{RuleID: "web", Name: "web"},
	)
	_, got, err := newWriteToolHandlers(client).deleteForwardingRule(context.Background(), nil, deleteForwardingRuleInput{RuleID: " web "})
	if err != nil {
		t.Fatalf("deleteForwardingRule returned error: %v", err)
	}
	if got.Action != "deleted" || !got.Applied {
		t.Fatalf("unexpected delete output: %+v", got)
	}
	draft := client.applySeen[0]
	if len(draft.Rules) != 1 || draft.Rules[0].RuleID != "ssh" {
		t.Fatalf("draft after delete = %+v", draft.Rules)
	}
	if got.Rule == nil || got.Rule.RuleID != "web" {
		t.Fatalf("deleted rule was not reported: %+v", got.Rule)
	}
}

func TestDeleteMissingRuleFails(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig(engine.Rule{RuleID: "ssh", Name: "ssh"})
	_, _, err := newWriteToolHandlers(client).deleteForwardingRule(context.Background(), nil, deleteForwardingRuleInput{RuleID: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want missing-rule error", err)
	}
	if client.applyCalls != 0 {
		t.Fatalf("missing rule delete reached the daemon: %d calls", client.applyCalls)
	}
}

func TestSetForwardingRuleEnabled(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig(engine.Rule{RuleID: "web", Name: "web", Enabled: true})

	_, got, err := newWriteToolHandlers(client).setForwardingRuleEnabled(context.Background(), nil, setForwardingRuleEnabledInput{RuleID: "web", Enabled: false})
	if err != nil {
		t.Fatalf("setForwardingRuleEnabled returned error: %v", err)
	}
	if got.Action != "disabled" || !got.Applied {
		t.Fatalf("unexpected disable output: %+v", got)
	}
	if rule := client.applySeen[0].Rules[0]; rule.Enabled {
		t.Fatalf("rule still enabled after disable: %+v", rule)
	}

	// Re-disabling is a no-op that must not reach the daemon. The fake does
	// not persist applies, so mirror the daemon's committed state first.
	client.applyCalls = 0
	client.config.Rules[0].Enabled = false
	_, got, err = newWriteToolHandlers(client).setForwardingRuleEnabled(context.Background(), nil, setForwardingRuleEnabledInput{RuleID: "web", Enabled: false})
	if err != nil {
		t.Fatalf("no-op setForwardingRuleEnabled returned error: %v", err)
	}
	if got.Action != "unchanged" || got.Applied || client.applyCalls != 0 {
		t.Fatalf("no-op output = %+v, apply calls = %d", got, client.applyCalls)
	}
}

func TestWriteErrorsAreActionable(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusPreconditionFailed, "concurrently"},
		{http.StatusForbidden, "admin management token"},
		{http.StatusUnprocessableEntity, "precheck"},
		{http.StatusServiceUnavailable, "rolled back"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			client := newFakeManagementClient()
			client.config = writeTestConfig()
			client.applyErr = &controlapi.APIError{StatusCode: tc.status, Message: "daemon said no"}
			_, _, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, baseUpsertInput())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "daemon said no") {
				t.Fatalf("daemon message was dropped: %v", err)
			}
		})
	}
}

func TestWriteRequiresWritableConfig(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	client.config.Writable = false
	_, _, err := newWriteToolHandlers(client).upsertForwardingRule(context.Background(), nil, baseUpsertInput())
	if err == nil || !strings.Contains(err.Error(), "cannot write the daemon configuration") {
		t.Fatalf("error = %v, want writable error", err)
	}
	if client.applyCalls != 0 {
		t.Fatalf("write reached the daemon despite read-only config")
	}
}

func TestWriteToolsFailWithoutWriteBackend(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	handlers := newToolHandlers(client)
	_, _, err := handlers.upsertForwardingRule(context.Background(), nil, baseUpsertInput())
	if err == nil || !strings.Contains(err.Error(), "write mode is disabled") {
		t.Fatalf("error = %v, want write-disabled error", err)
	}
}

func TestServerWriteModeAdvertisesWriteTools(t *testing.T) {
	session := connectClient(t, NewServer(newFakeManagementClient(), "v1.2.3", Options{WriteEnabled: true}))
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools returned error: %v", err)
	}
	annotations := map[string]*mcp.ToolAnnotations{}
	for _, tool := range result.Tools {
		annotations[tool.Name] = tool.Annotations
	}
	for _, name := range []string{toolUpsertForwardingRule, toolDeleteForwardingRule, toolSetForwardingRuleEnabled} {
		if _, ok := annotations[name]; !ok {
			t.Fatalf("write tool %q missing in write mode", name)
		}
	}
	for name, annotation := range annotations {
		switch name {
		case toolUpsertForwardingRule, toolDeleteForwardingRule, toolSetForwardingRuleEnabled:
			if annotation == nil || annotation.ReadOnlyHint {
				t.Fatalf("write tool %q is annotated read-only", name)
			}
		default:
			if annotation == nil || !annotation.ReadOnlyHint {
				t.Fatalf("read tool %q lost its read-only annotation", name)
			}
		}
	}
	if delete := annotations[toolDeleteForwardingRule]; delete == nil || delete.DestructiveHint == nil || !*delete.DestructiveHint {
		t.Fatalf("delete tool missing destructive annotation: %+v", delete)
	}
	if upsert := annotations[toolUpsertForwardingRule]; upsert == nil || !upsert.IdempotentHint {
		t.Fatalf("upsert tool missing idempotent annotation: %+v", upsert)
	}
}

func TestStatusReportsWriteModeAndCapabilityGap(t *testing.T) {
	client := newFakeManagementClient()
	client.config = writeTestConfig()
	client.session = &controlapi.SessionResponse{APIVersion: "1", ServerVersion: "v9.9.9", Capabilities: controlapi.SessionCapabilities{RulesWrite: false}}

	_, got, err := newWriteToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("getVMFlowStatus returned error: %v", err)
	}
	if got.MCPReadOnly {
		t.Fatalf("write-mode status reported read-only: %+v", got)
	}
	if len(got.Issues) == 0 || !strings.Contains(strings.Join(got.Issues, "; "), "admin management token") {
		t.Fatalf("capability gap was not reported: %v", got.Issues)
	}

	client.session = &controlapi.SessionResponse{APIVersion: "1", ServerVersion: "v9.9.9", Capabilities: controlapi.SessionCapabilities{RulesWrite: true}}
	_, got, err = newWriteToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("getVMFlowStatus returned error: %v", err)
	}
	if len(got.Issues) != 0 {
		t.Fatalf("unexpected issues with write capability: %v", got.Issues)
	}
}

func TestWriteModeInstructionsWithoutWriteClientStayReadOnly(t *testing.T) {
	readOnlyClient := &readOnlyOnlyClient{fake: newFakeManagementClient()}
	session := connectClient(t, NewServer(readOnlyClient, "v1.2.3", Options{WriteEnabled: true}))
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools returned error: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name == toolUpsertForwardingRule || tool.Name == toolDeleteForwardingRule || tool.Name == toolSetForwardingRuleEnabled {
			t.Fatalf("write tool %q advertised without a write-capable client", tool.Name)
		}
	}
}

// readOnlyOnlyClient wraps the fake so it only satisfies the read interface,
// simulating a management client that cannot write.
type readOnlyOnlyClient struct {
	fake *fakeManagementClient
}

func (r readOnlyOnlyClient) Session(ctx context.Context) (*controlapi.SessionResponse, error) {
	return r.fake.Session(ctx)
}

func (r readOnlyOnlyClient) ConfigRules(ctx context.Context) (*controlapi.ConfigRulesResponse, error) {
	return r.fake.ConfigRules(ctx)
}

func (r readOnlyOnlyClient) Rules(ctx context.Context) (*controlapi.RulesResponse, error) {
	return r.fake.Rules(ctx)
}

func (r readOnlyOnlyClient) Stats(ctx context.Context) (*controlapi.StatsResponse, error) {
	return r.fake.Stats(ctx)
}

func (r readOnlyOnlyClient) CurrentPrecheck(ctx context.Context) (*controlapi.CurrentPrecheckResponse, error) {
	return r.fake.CurrentPrecheck(ctx)
}
