package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestStatsPollingRejectsStaleSuccessAndError(t *testing.T) {
	m := newModel(t.Context(), "http://127.0.0.1:19090")
	first := &StatsResponse{Items: []TrafficSnapshot{{RuleID: "one", UploadBytes: 10, UpdatedTime: 10}}}

	updated, _ := m.Update(statsMsg{requestID: m.statsRequestID, resp: first})
	m = updated.(Model)
	if m.stats != first || m.lastStatsAt.IsZero() || m.connectionErr != nil {
		t.Fatalf("first response state: stats=%p last=%v err=%v", m.stats, m.lastStatsAt, m.connectionErr)
	}
	firstAcceptedAt := m.lastStatsAt

	latestRequestID := m.beginStatsRequest()
	staleErr := errors.New("stale failure")
	updated, _ = m.Update(statsMsg{requestID: latestRequestID - 1, err: staleErr})
	m = updated.(Model)
	if m.stats != first || m.connectionErr != nil || !m.lastStatsAt.Equal(firstAcceptedAt) {
		t.Fatalf("stale error changed state: stats=%p last=%v err=%v", m.stats, m.lastStatsAt, m.connectionErr)
	}

	latestErr := errors.New("latest failure")
	updated, _ = m.Update(statsMsg{requestID: latestRequestID, err: latestErr})
	m = updated.(Model)
	if m.stats != first || !errors.Is(m.connectionErr, latestErr) || !m.lastStatsAt.Equal(firstAcceptedAt) {
		t.Fatalf("latest error did not preserve last success: stats=%p last=%v err=%v", m.stats, m.lastStatsAt, m.connectionErr)
	}

	second := &StatsResponse{Items: []TrafficSnapshot{{RuleID: "one", UploadBytes: 20, UpdatedTime: 20}}}
	latestRequestID = m.beginStatsRequest()
	updated, _ = m.Update(statsMsg{requestID: latestRequestID, resp: second})
	m = updated.(Model)
	if m.stats != second || m.connectionErr != nil || m.lastStatsAt.Before(firstAcceptedAt) {
		t.Fatalf("latest success state: stats=%p last=%v err=%v", m.stats, m.lastStatsAt, m.connectionErr)
	}

	updated, _ = m.Update(statsMsg{requestID: latestRequestID - 1, resp: first})
	m = updated.(Model)
	if m.stats != second {
		t.Fatal("stale success replaced the latest stats")
	}
}

func TestRulesAndSessionPollingRejectStaleResponses(t *testing.T) {
	m := newModel(t.Context(), "http://127.0.0.1:19090")
	currentRules := &RulesResponse{Items: []RuleInfo{{RuleID: "current"}}}
	currentSession := &SessionResponse{Actor: "current", Role: "admin"}

	updated, _ := m.Update(rulesMsg{requestID: m.rulesRequestID, resp: currentRules})
	m = updated.(Model)
	updated, _ = m.Update(sessionMsg{requestID: m.sessionRequestID, resp: currentSession})
	m = updated.(Model)

	rulesRequestID := m.beginRulesRequest()
	sessionRequestID := m.beginSessionRequest()
	staleErr := errors.New("stale failure")
	updated, _ = m.Update(rulesMsg{requestID: rulesRequestID - 1, err: staleErr})
	m = updated.(Model)
	updated, _ = m.Update(sessionMsg{requestID: sessionRequestID - 1, err: staleErr})
	m = updated.(Model)
	if m.rules != currentRules || m.rulesErr != nil {
		t.Fatalf("stale rules error changed state: rules=%p err=%v", m.rules, m.rulesErr)
	}
	if m.session != currentSession || m.sessionErr != nil {
		t.Fatalf("stale session error changed state: session=%p err=%v", m.session, m.sessionErr)
	}

	latestErr := errors.New("latest failure")
	updated, _ = m.Update(rulesMsg{requestID: rulesRequestID, err: latestErr})
	m = updated.(Model)
	updated, _ = m.Update(sessionMsg{requestID: sessionRequestID, err: latestErr})
	m = updated.(Model)
	if m.rules != currentRules || !errors.Is(m.rulesErr, latestErr) {
		t.Fatalf("latest rules error did not preserve data: rules=%p err=%v", m.rules, m.rulesErr)
	}
	if m.session != currentSession || !errors.Is(m.sessionErr, latestErr) {
		t.Fatalf("latest session error did not preserve data: session=%p err=%v", m.session, m.sessionErr)
	}

	newRules := &RulesResponse{Items: []RuleInfo{{RuleID: "new"}}}
	newSession := &SessionResponse{Actor: "new", Role: "viewer"}
	rulesRequestID = m.beginRulesRequest()
	sessionRequestID = m.beginSessionRequest()
	updated, _ = m.Update(rulesMsg{requestID: rulesRequestID, resp: newRules})
	m = updated.(Model)
	updated, _ = m.Update(sessionMsg{requestID: sessionRequestID, resp: newSession})
	m = updated.(Model)

	updated, _ = m.Update(rulesMsg{requestID: rulesRequestID - 1, resp: currentRules})
	m = updated.(Model)
	updated, _ = m.Update(sessionMsg{requestID: sessionRequestID - 1, resp: currentSession})
	m = updated.(Model)
	if m.rules != newRules || m.rulesErr != nil {
		t.Fatalf("stale rules success changed state: rules=%p err=%v", m.rules, m.rulesErr)
	}
	if m.session != newSession || m.sessionErr != nil {
		t.Fatalf("stale session success changed state: session=%p err=%v", m.session, m.sessionErr)
	}
}

func TestTickAndRefreshAdvanceOnlyRequestedGenerations(t *testing.T) {
	m := newModel(t.Context(), "http://127.0.0.1:19090")

	updated, cmd := m.Update(tickMsg(time.Now()))
	m = updated.(Model)
	if m.statsRequestID != 2 || m.rulesRequestID != 2 || m.sessionRequestID != 1 || m.configRequestID != 2 {
		t.Fatalf("tick generations: stats=%d rules=%d session=%d config=%d", m.statsRequestID, m.rulesRequestID, m.sessionRequestID, m.configRequestID)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 4 {
		t.Fatalf("tick batch = %T len=%d, want four commands including one timer", cmd(), len(batch))
	}

	updated, cmd = m.refreshNow()
	m = updated.(Model)
	if m.statsRequestID != 3 || m.rulesRequestID != 3 || m.sessionRequestID != 2 || m.configRequestID != 3 {
		t.Fatalf("refresh generations: stats=%d rules=%d session=%d config=%d", m.statsRequestID, m.rulesRequestID, m.sessionRequestID, m.configRequestID)
	}
	batch, ok = cmd().(tea.BatchMsg)
	if !ok || len(batch) != 4 {
		t.Fatalf("refresh batch = %T len=%d, want four requests", cmd(), len(batch))
	}
}

func TestComputeRatesUseLocalSampleIntervalAndCleanMissingRules(t *testing.T) {
	m := newModel(t.Context(), "http://127.0.0.1:19090")
	m.lastStatsAt = time.Unix(100, 0)
	m.stats = &StatsResponse{Items: []TrafficSnapshot{
		{RuleID: "idle", UploadBytes: 100, DownloadBytes: 200, UpdatedTime: 10},
		{RuleID: "active", UploadBytes: 100, DownloadBytes: 200, UpdatedTime: 10},
		{RuleID: "reset", UploadBytes: 500, DownloadBytes: 800, UpdatedTime: 10},
		{RuleID: "gone", UploadBytes: 10, DownloadBytes: 10, UpdatedTime: 10},
	}}
	m.rates["idle"] = rateEntry{UploadRate: 50, DownloadRate: 60}
	m.rates["gone"] = rateEntry{UploadRate: 1, DownloadRate: 1}
	m.history["idle"] = &rateHistory{uploadRates: []float64{50}, downloadRates: []float64{60}}
	m.history["gone"] = &rateHistory{uploadRates: []float64{1}, downloadRates: []float64{1}}

	m.computeRates(&StatsResponse{Items: []TrafficSnapshot{
		{RuleID: "idle", UploadBytes: 100, DownloadBytes: 200, UpdatedTime: 10},
		{RuleID: "active", UploadBytes: 150, DownloadBytes: 260, UpdatedTime: 10},
		{RuleID: "reset", UploadBytes: 10, DownloadBytes: 820, UpdatedTime: 12},
		{RuleID: "new", UploadBytes: 40, DownloadBytes: 50, UpdatedTime: 12},
	}}, time.Unix(102, 0))

	if got := m.rates["idle"]; got.UploadRate != 0 || got.DownloadRate != 0 {
		t.Fatalf("idle rate = %+v, want zero", got)
	}
	if got := m.rates["active"]; got.UploadRate != 25 || got.DownloadRate != 30 {
		t.Fatalf("active rate = %+v, want upload 25 and download 30", got)
	}
	if got := m.rates["reset"]; got.UploadRate != 0 || got.DownloadRate != 10 {
		t.Fatalf("reset rate = %+v, want upload zero and download 10", got)
	}
	if got := m.rates["new"]; got.UploadRate != 0 || got.DownloadRate != 0 {
		t.Fatalf("new rule rate = %+v, want zero", got)
	}
	if _, exists := m.rates["gone"]; exists {
		t.Fatal("missing rule rate was not removed")
	}
	if _, exists := m.history["gone"]; exists {
		t.Fatal("missing rule history was not removed")
	}
	if got := m.history["idle"]; got == nil || len(got.uploadRates) != 2 || got.uploadRates[1] != 0 || got.downloadRates[1] != 0 {
		t.Fatalf("idle history = %+v, want appended zero sample", got)
	}
	if got := m.history["new"]; got == nil || len(got.uploadRates) != 1 || got.uploadRates[0] != 0 || got.downloadRates[0] != 0 {
		t.Fatalf("new rule history = %+v, want initial zero sample", got)
	}
}
