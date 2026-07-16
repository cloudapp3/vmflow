package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

type fakeManagementClient struct {
	session  *controlapi.SessionResponse
	config   *controlapi.ConfigRulesResponse
	rules    *controlapi.RulesResponse
	stats    *controlapi.StatsResponse
	precheck *controlapi.CurrentPrecheckResponse

	sessionErr  error
	configErr   error
	rulesErr    error
	statsErr    error
	precheckErr error

	sessionCalls  int
	configCalls   int
	rulesCalls    int
	statsCalls    int
	precheckCalls int
}

func newFakeManagementClient() *fakeManagementClient {
	return &fakeManagementClient{
		session:  &controlapi.SessionResponse{},
		config:   &controlapi.ConfigRulesResponse{Rules: []engine.Rule{}},
		rules:    &controlapi.RulesResponse{Items: []engine.Rule{}},
		stats:    &controlapi.StatsResponse{Items: []controlapi.TrafficSnapshot{}},
		precheck: &controlapi.CurrentPrecheckResponse{Result: precheck.Result{Items: []precheck.Item{}}},
	}
}

func (f *fakeManagementClient) Session(context.Context) (*controlapi.SessionResponse, error) {
	f.sessionCalls++
	return f.session, f.sessionErr
}

func (f *fakeManagementClient) ConfigRules(context.Context) (*controlapi.ConfigRulesResponse, error) {
	f.configCalls++
	return f.config, f.configErr
}

func (f *fakeManagementClient) Rules(context.Context) (*controlapi.RulesResponse, error) {
	f.rulesCalls++
	return f.rules, f.rulesErr
}

func (f *fakeManagementClient) Stats(context.Context) (*controlapi.StatsResponse, error) {
	f.statsCalls++
	return f.stats, f.statsErr
}

func (f *fakeManagementClient) CurrentPrecheck(context.Context) (*controlapi.CurrentPrecheckResponse, error) {
	f.precheckCalls++
	return f.precheck, f.precheckErr
}

func newToolHandlers(client ManagementClient) *toolHandlers {
	return newToolHandlersForBackend(newManagementBackend(client), "vtest")
}

func TestGetVMFlowStatusAggregatesDaemonState(t *testing.T) {
	client := newFakeManagementClient()
	client.session = &controlapi.SessionResponse{
		Actor:         "mcp-viewer",
		Role:          "viewer",
		APIVersion:    "1",
		ServerVersion: "v2.3.4",
		Commit:        "abc123",
		StartedTime:   1234,
		Degraded:      true,
		DegradedCause: "configuration drift",
	}
	client.config = &controlapi.ConfigRulesResponse{
		Revision:       "rev-1",
		Writable:       false,
		UDPMaxSessions: 512,
		Rules: []engine.Rule{
			{RuleID: "enabled", Enabled: true},
			{RuleID: "disabled", Enabled: false},
		},
	}
	client.rules = &controlapi.RulesResponse{Items: []engine.Rule{{RuleID: "enabled"}}}
	client.stats = &controlapi.StatsResponse{Items: []controlapi.TrafficSnapshot{
		{RuleID: "enabled", UploadBytes: 10, DownloadBytes: 20, Conns: 2, SourceIPDenied: 3, UDPSessionRejected: 4, UDPPacketsDropped: 5},
		{RuleID: "removed", UploadBytes: 1, DownloadBytes: 2, Conns: 1},
	}}

	_, got, err := newToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("getVMFlowStatus returned error: %v", err)
	}
	if !got.Connected || got.DaemonVersion != "v2.3.4" || got.APIVersion != "1" || got.ConfigRevision != "rev-1" {
		t.Fatalf("unexpected status metadata: %+v", got)
	}
	if !got.MCPReadOnly {
		t.Fatalf("status did not declare MCP read-only: %+v", got)
	}
	if !got.Degraded || got.DegradedCause != "configuration drift" || len(got.Issues) != 0 {
		t.Fatalf("unexpected status health: %+v", got)
	}
	want := statusCounters{
		ConfiguredRules: 2, EnabledRules: 1, DisabledRules: 1, RuntimeRules: 1, TrafficRules: 2,
		UploadBytes: 11, DownloadBytes: 22, ActiveConnections: 3,
		SourceIPDenied: 3, UDPSessionRejected: 4, UDPPacketsDropped: 5,
	}
	if got.Counters != want {
		t.Fatalf("counters = %+v, want %+v", got.Counters, want)
	}
}

func TestGetVMFlowStatusPreservesPartialFailures(t *testing.T) {
	client := newFakeManagementClient()
	client.session.ServerVersion = "vtest"
	client.session.APIVersion = controlapi.ManagementAPIVersion
	client.configErr = errors.New("forbidden")
	client.rules = nil
	client.statsErr = errors.New("stats unavailable")

	_, got, err := newToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("getVMFlowStatus returned error: %v", err)
	}
	if !got.Connected || len(got.Issues) != 3 {
		t.Fatalf("partial status = %+v", got)
	}
}

func TestGetVMFlowStatusDistinguishesHTTPFailuresFromOfflineDaemon(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{status: 401, want: "authentication failed"},
		{status: 403, want: "access was denied"},
		{status: 404, want: "too old"},
		{status: 500, want: "HTTP 500"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("status_%d", test.status), func(t *testing.T) {
			client := newFakeManagementClient()
			client.sessionErr = &controlapi.APIError{StatusCode: test.status, Message: "daemon response"}

			_, got, err := newToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
			if err != nil {
				t.Fatalf("getVMFlowStatus returned error: %v", err)
			}
			if !got.Connected || len(got.Issues) != 1 || !strings.Contains(got.Issues[0], test.want) {
				t.Fatalf("HTTP failure status = %+v", got)
			}
			if client.configCalls != 0 || client.rulesCalls != 0 || client.statsCalls != 0 {
				t.Fatalf("HTTP failure made follow-up calls: config=%d rules=%d stats=%d", client.configCalls, client.rulesCalls, client.statsCalls)
			}
		})
	}
}

func TestGetVMFlowStatusReportsLegacyAndIncompatibleDaemonMetadata(t *testing.T) {
	client := newFakeManagementClient()
	_, legacy, err := newToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("legacy status returned error: %v", err)
	}
	if len(legacy.Issues) != 2 || !strings.Contains(strings.Join(legacy.Issues, " "), "API version") || !strings.Contains(strings.Join(legacy.Issues, " "), "daemon version") {
		t.Fatalf("legacy status issues = %v", legacy.Issues)
	}

	client.session.APIVersion = "999"
	client.session.ServerVersion = "vtest"
	_, incompatible, err := newToolHandlers(client).getVMFlowStatus(context.Background(), nil, emptyInput{})
	if err != nil {
		t.Fatalf("incompatible status returned error: %v", err)
	}
	if len(incompatible.Issues) != 1 || !strings.Contains(incompatible.Issues[0], "incompatible") {
		t.Fatalf("incompatible status issues = %v", incompatible.Issues)
	}
}

func TestListForwardingRulesFiltersSortsLimitsAndRedacts(t *testing.T) {
	client := newFakeManagementClient()
	client.config = &controlapi.ConfigRulesResponse{
		Revision: "rev-2",
		Rules: []engine.Rule{
			{RuleID: "z", Name: "Zulu", Protocol: engine.ProtocolTCP, Enabled: true, ListenAddr: "0.0.0.0", TargetAddr: "secret.internal", SourceIPs: []string{"192.0.2.0/24"}, Domains: []string{"secret.example"}},
			{RuleID: "a2", Name: "alpha", Protocol: engine.ProtocolUDP, Enabled: true, ListenAddr: "127.0.0.1", TargetAddr: "192.0.2.2"},
			{RuleID: "a1", Name: "Alpha", Protocol: engine.ProtocolUDP, Enabled: true, ListenAddr: "127.0.0.1", TargetAddr: "192.0.2.1"},
			{RuleID: "off", Name: "Alpha disabled", Protocol: engine.ProtocolUDP, Enabled: false},
		},
	}
	enabled := true
	_, got, err := newToolHandlers(client).listForwardingRules(context.Background(), nil, listForwardingRulesInput{
		Query: "alpha", Protocol: "UDP", Enabled: &enabled, Limit: 1,
	})
	if err != nil {
		t.Fatalf("listForwardingRules returned error: %v", err)
	}
	if got.Total != 4 || got.Matched != 2 || got.Returned != 1 || got.Rules[0].RuleID != "a1" {
		t.Fatalf("unexpected list result: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	for _, forbidden := range []string{"listen_addr", "listen_port", "target_addr", "target_port", "source_ips", "domains", "secret.internal", "secret.example"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("list output leaked %q: %s", forbidden, raw)
		}
	}
	_, got, err = newToolHandlers(client).listForwardingRules(context.Background(), nil, listForwardingRulesInput{Query: "a2"})
	if err != nil || got.Matched != 1 || got.Rules[0].RuleID != "a2" {
		t.Fatalf("rule ID query = %+v, err=%v", got, err)
	}
}

func TestListForwardingRulesNormalizesEmptyCollectionAndRejectsOptions(t *testing.T) {
	client := newFakeManagementClient()
	client.config.Rules = nil
	handlers := newToolHandlers(client)
	_, got, err := handlers.listForwardingRules(context.Background(), nil, listForwardingRulesInput{})
	if err != nil {
		t.Fatalf("listForwardingRules returned error: %v", err)
	}
	if got.Rules == nil || got.Returned != 0 {
		t.Fatalf("empty list was not normalized: %+v", got)
	}
	for _, input := range []listForwardingRulesInput{
		{Protocol: "quic"},
		{Limit: -1},
		{Limit: maxListLimit + 1},
		{Query: strings.Repeat("x", maxFilterLength+1)},
	} {
		if _, _, err := handlers.listForwardingRules(context.Background(), nil, input); err == nil {
			t.Fatalf("input %+v returned nil error", input)
		}
	}
}

func TestGetForwardingRuleReturnsFullRuleAndNormalizesSlices(t *testing.T) {
	client := newFakeManagementClient()
	client.config = &controlapi.ConfigRulesResponse{Revision: "rev-3", Rules: []engine.Rule{{
		RuleID: "web", Name: "web", Protocol: engine.ProtocolHTTPS,
		ListenAddr: "0.0.0.0", ListenPort: 443,
		TargetAddr: "10.0.0.2", TargetPort: 8443,
		SourceIPMode: engine.SourceIPModeAllowlist,
	}}}
	client.rules = &controlapi.RulesResponse{Items: []engine.Rule{{RuleID: "web"}}}
	client.stats = &controlapi.StatsResponse{Items: []controlapi.TrafficSnapshot{{RuleID: "web", UploadBytes: 10}}}
	handlers := newToolHandlers(client)
	_, got, err := handlers.getForwardingRule(context.Background(), nil, getForwardingRuleInput{RuleID: "web"})
	if err != nil {
		t.Fatalf("getForwardingRule returned error: %v", err)
	}
	if got.Rule.ListenAddr != "0.0.0.0" || got.Rule.TargetAddr != "10.0.0.2" || got.Rule.SourceIPs == nil || got.Rule.Domains == nil || !got.Running || !got.StatsAvailable || got.Stats.UploadBytes != 10 {
		t.Fatalf("full rule = %+v", got)
	}
	if _, _, err := handlers.getForwardingRule(context.Background(), nil, getForwardingRuleInput{RuleID: "missing"}); err == nil {
		t.Fatal("missing rule returned nil error")
	}
}

func TestGetTrafficStatsSortsFiltersLimitsAndAggregates(t *testing.T) {
	client := newFakeManagementClient()
	client.stats = &controlapi.StatsResponse{Items: []controlapi.TrafficSnapshot{
		{RuleID: "z", UploadBytes: 10, DownloadBytes: 20, Conns: 2},
		{RuleID: "a", UploadBytes: 1, DownloadBytes: 2, Conns: 1, SourceIPDenied: 3, UDPSessionRejected: 4, UDPPacketsDropped: 5},
	}}
	handlers := newToolHandlers(client)
	_, got, err := handlers.getTrafficStats(context.Background(), nil, getTrafficStatsInput{Limit: 1})
	if err != nil {
		t.Fatalf("getTrafficStats returned error: %v", err)
	}
	if got.Total != 2 || got.Matched != 2 || got.Returned != 1 || got.Items[0].RuleID != "a" {
		t.Fatalf("unexpected stats result: %+v", got)
	}
	if got.Totals.UploadBytes != 11 || got.Totals.DownloadBytes != 22 || got.Totals.ActiveConnections != 3 || got.Totals.SourceIPDenied != 3 || got.Totals.UDPSessionRejected != 4 || got.Totals.UDPPacketsDropped != 5 {
		t.Fatalf("unexpected totals: %+v", got.Totals)
	}
	_, got, err = handlers.getTrafficStats(context.Background(), nil, getTrafficStatsInput{RuleID: "z"})
	if err != nil || got.Matched != 1 || got.Items[0].RuleID != "z" || got.Totals.UploadBytes != 10 {
		t.Fatalf("filtered stats = %+v, err=%v", got, err)
	}
}

func TestGetTrafficStatsNormalizesEmptyCollection(t *testing.T) {
	client := newFakeManagementClient()
	client.stats.Items = nil
	_, got, err := newToolHandlers(client).getTrafficStats(context.Background(), nil, getTrafficStatsInput{})
	if err != nil {
		t.Fatalf("getTrafficStats returned error: %v", err)
	}
	if got.Items == nil {
		t.Fatalf("empty stats were not normalized: %+v", got)
	}
}

func TestRunConfigPrecheckReturnsStructuredFailureAndNormalizesItems(t *testing.T) {
	client := newFakeManagementClient()
	client.precheck = &controlapi.CurrentPrecheckResponse{
		ConfigPath: "/etc/vmflow/config.yaml",
		Error:      "invalid configuration",
		Result: precheck.Result{
			OK:         false,
			ErrorCount: 1,
			Items:      nil,
		},
	}
	_, got, err := newToolHandlers(client).runConfigPrecheck(context.Background(), nil, runConfigPrecheckInput{})
	if err != nil {
		t.Fatalf("runConfigPrecheck returned error: %v", err)
	}
	if got.Error == "" || got.Result.Items == nil || got.Total != 0 || got.Returned != 0 || got.Truncated {
		t.Fatalf("precheck result = %+v", got)
	}
}

func TestRunConfigPrecheckLimitsFindingsWithoutMutatingBackend(t *testing.T) {
	client := newFakeManagementClient()
	items := make([]precheck.Item, maxListLimit+1)
	for index := range items {
		items[index] = precheck.Item{Severity: precheck.SeverityWarning, Check: "test", Message: "finding"}
	}
	client.precheck.Result.Items = items

	_, got, err := newToolHandlers(client).runConfigPrecheck(context.Background(), nil, runConfigPrecheckInput{})
	if err != nil {
		t.Fatalf("runConfigPrecheck returned error: %v", err)
	}
	if got.Total != len(items) || got.Returned != defaultListLimit || !got.Truncated || len(got.Result.Items) != defaultListLimit {
		t.Fatalf("limited precheck = %+v", got)
	}
	if len(client.precheck.Result.Items) != len(items) {
		t.Fatalf("backend result was mutated: len=%d, want %d", len(client.precheck.Result.Items), len(items))
	}
	if _, _, err := newToolHandlers(client).runConfigPrecheck(context.Background(), nil, runConfigPrecheckInput{Limit: maxListLimit + 1}); err == nil {
		t.Fatal("oversized precheck limit returned nil error")
	}
}

type blockingBackend struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
}

func (b *blockingBackend) Session(context.Context) (*controlapi.SessionResponse, error) {
	return &controlapi.SessionResponse{}, nil
}

func (b *blockingBackend) ConfigRules(context.Context) (*controlapi.ConfigRulesResponse, error) {
	return &controlapi.ConfigRulesResponse{Rules: []engine.Rule{}}, nil
}

func (b *blockingBackend) Rules(context.Context) (*controlapi.RulesResponse, error) {
	return &controlapi.RulesResponse{Items: []engine.Rule{}}, nil
}

func (b *blockingBackend) Stats(ctx context.Context) (*controlapi.StatsResponse, error) {
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		maximum := b.maximum.Load()
		if active <= maximum || b.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return &controlapi.StatsResponse{Items: []controlapi.TrafficSnapshot{}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingBackend) CurrentPrecheck(context.Context) (*controlapi.CurrentPrecheckResponse, error) {
	return &controlapi.CurrentPrecheckResponse{Result: precheck.Result{Items: []precheck.Item{}}}, nil
}

func TestToolHandlersLimitConcurrentBackendCalls(t *testing.T) {
	backend := &blockingBackend{
		entered: make(chan struct{}, maxConcurrentTools+1),
		release: make(chan struct{}),
	}
	handlers := newToolHandlersForBackend(backend, "vtest")
	var wg sync.WaitGroup
	errors := make(chan error, maxConcurrentTools+1)
	for range maxConcurrentTools + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := handlers.getTrafficStats(context.Background(), nil, getTrafficStatsInput{})
			errors <- err
		}()
	}
	for range maxConcurrentTools {
		select {
		case <-backend.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for backend call")
		}
	}
	select {
	case <-backend.entered:
		t.Fatal("fifth backend call bypassed concurrency gate")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.release)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("getTrafficStats returned error: %v", err)
		}
	}
	if got := backend.maximum.Load(); got > maxConcurrentTools {
		t.Fatalf("maximum concurrent backend calls = %d, want <= %d", got, maxConcurrentTools)
	}
}
