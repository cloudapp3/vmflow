package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/tgbot/ext"
	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/engine"
)

// fakeControl is a minimal control API stand-in for executor tests. It serves
// GET/PUT /v1/config/rules and POST /v1/reload, recording the last PUT body.
type fakeControl struct {
	rules     []engine.Rule
	revision  string
	udpMax    int
	putStatus int // non-zero makes PUT return this status instead of 200
	lastPut   *controlapi.ConfigRulesRequest
	putCount  int
}

func newFakeControlServer(t *testing.T, f *fakeControl) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config/rules":
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("ETag", `"`+f.revision+`"`)
				writeTestJSON(w, http.StatusOK, map[string]any{
					"revision":         f.revision,
					"writable":         true,
					"udp_max_sessions": f.udpMax,
					"rules":            f.rules,
				})
			case http.MethodPut:
				var req controlapi.ConfigRulesRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				f.lastPut = &req
				f.putCount++
				if f.putStatus != 0 {
					writeTestJSON(w, f.putStatus, map[string]any{"error": "fail"})
					return
				}
				f.rules = req.Rules
				writeTestJSON(w, http.StatusOK, map[string]any{
					"revision": f.revision,
					"result":   map[string]any{},
				})
			default:
				writeTestJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			}
		case "/v1/reload":
			writeTestJSON(w, http.StatusOK, map[string]any{"rule_count": len(f.rules)})
		default:
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		}
	}))
}

type telegramCall struct {
	method string
	body   map[string]any
}

type telegramRecorder struct {
	mu         sync.Mutex
	calls      []telegramCall
	failMethod string
}

func newFakeTelegramServer(t *testing.T, recorder *telegramRecorder) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bottest-token/")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Telegram %s request: %v", method, err)
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "description": "bad request"})
			return
		}
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, telegramCall{method: method, body: body})
		fail := method == recorder.failMethod
		recorder.mu.Unlock()
		if fail {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{
				"ok":          false,
				"error_code":  400,
				"description": "forced failure",
			})
			return
		}
		result := any(true)
		if method == "sendMessage" {
			result = map[string]any{
				"message_id": 1,
				"date":       1,
				"chat":       map[string]any{"id": body["chat_id"], "type": "private"},
			}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
	}))
}

func newTestTelegramBot(t *testing.T, serverURL string) *tg.Bot {
	t.Helper()
	client, err := tg.NewBot("test-token", tg.WithAPIURL(serverURL))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return client
}

func (recorder *telegramRecorder) callsFor(method string) []telegramCall {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var calls []telegramCall
	for _, call := range recorder.calls {
		if call.method == method {
			calls = append(calls, call)
		}
	}
	return calls
}

func commandContext(chatID int64, text string) *ext.Context {
	return &ext.Context{Update: &ext.Update{Message: &tg.Message{
		Chat: &tg.Chat{ID: chatID, Type: "private"},
		Text: text,
	}}}
}

func callbackContext(chatID, userID int64, messageID int64, data string) *ext.Context {
	return &ext.Context{Update: &ext.Update{CallbackQuery: &tg.CallbackQuery{
		ID:   "callback-id",
		From: &tg.User{ID: userID},
		Message: &tg.Message{
			MessageID:       messageID,
			MessageThreadID: 17,
			Chat:            &tg.Chat{ID: chatID, Type: "supergroup"},
		},
		Data: data,
	}}}
}

func callbackDataFromSend(t *testing.T, call telegramCall) string {
	t.Helper()
	markup, ok := call.body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("reply_markup = %#v", call.body["reply_markup"])
	}
	rows, ok := markup["inline_keyboard"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("inline_keyboard = %#v", markup["inline_keyboard"])
	}
	row, ok := rows[0].([]any)
	if !ok || len(row) == 0 {
		t.Fatalf("first keyboard row = %#v", rows[0])
	}
	button, ok := row[0].(map[string]any)
	if !ok {
		t.Fatalf("first button = %#v", row[0])
	}
	data, ok := button["callback_data"].(string)
	if !ok {
		t.Fatalf("callback_data = %#v", button["callback_data"])
	}
	return data
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDoStopRulePersistsDisabled(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "r1", Name: "r1", Protocol: engine.ProtocolTCP, Enabled: true}},
		revision: "sha256:abc",
		udpMax:   256,
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doStopRule(context.Background(), "r1")
	if !strings.Contains(got, "已停用") {
		t.Fatalf("doStopRule = %q", got)
	}
	if f.lastPut == nil || len(f.lastPut.Rules) != 1 || f.lastPut.Rules[0].Enabled != false {
		t.Fatalf("PUT body not as expected: %+v", f.lastPut)
	}
	if f.lastPut.UDPMaxSessions != 256 {
		t.Fatalf("UDPMaxSessions not carried forward: %d", f.lastPut.UDPMaxSessions)
	}
}

func TestDoStartRulePersistsEnabled(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "r1", Enabled: false}},
		revision: "sha256:abc",
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doStartRule(context.Background(), "r1")
	if !strings.Contains(got, "已启用") {
		t.Fatalf("doStartRule = %q", got)
	}
	if f.lastPut == nil || f.lastPut.Rules[0].Enabled != true {
		t.Fatalf("PUT did not enable rule: %+v", f.lastPut)
	}
}

func TestDoToggleRuleFlipsState(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "r1", Enabled: true}},
		revision: "sha256:abc",
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doToggleRule(context.Background(), "r1")
	if !strings.Contains(got, "已停用") {
		t.Fatalf("toggle of enabled rule = %q", got)
	}
}

func TestDoStopRuleConflictReportsRetry(t *testing.T) {
	f := &fakeControl{
		rules:     []engine.Rule{{RuleID: "r1", Enabled: true}},
		revision:  "sha256:abc",
		putStatus: http.StatusPreconditionFailed,
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doStopRule(context.Background(), "r1")
	if !strings.Contains(got, "配置已被其他途径修改") {
		t.Fatalf("conflict message = %q", got)
	}
}

func TestDoStopRuleNotFound(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "other", Enabled: true}},
		revision: "sha256:abc",
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doStopRule(context.Background(), "missing")
	if !strings.Contains(got, "规则不存在") {
		t.Fatalf("not-found message = %q", got)
	}
}

func TestDoStopRuleAlreadyInState(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "r1", Enabled: false}},
		revision: "sha256:abc",
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doStopRule(context.Background(), "r1")
	if !strings.Contains(got, "已是") {
		t.Fatalf("already-state message = %q", got)
	}
	if f.lastPut != nil {
		t.Fatalf("no PUT expected when rule already in state, got %+v", f.lastPut)
	}
}

func TestDoReload(t *testing.T) {
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: "r1"}, {RuleID: "r2"}},
		revision: "sha256:abc",
	}
	srv := newFakeControlServer(t, f)
	defer srv.Close()

	b := &Bot{control: controlapi.NewClient(srv.URL, "tok")}
	got := b.doReload(context.Background())
	if !strings.Contains(got, "reload 完成") || !strings.Contains(got, "2") {
		t.Fatalf("reload message = %q", got)
	}
}

func TestWriteCommandsDisabledWithoutToken(t *testing.T) {
	// nil control client → read-only bot.
	b := &Bot{control: nil}
	if got := b.doReload(context.Background()); !strings.Contains(got, "控制未启用") {
		t.Fatalf("nil doReload = %q", got)
	}
	if got := b.doStopRule(context.Background(), "r1"); !strings.Contains(got, "控制未启用") {
		t.Fatalf("nil doStopRule = %q", got)
	}
	if got := b.doToggleRule(context.Background(), "r1"); !strings.Contains(got, "控制未启用") {
		t.Fatalf("nil doToggleRule = %q", got)
	}

	// Non-nil client but empty token → also read-only.
	b2 := &Bot{control: controlapi.NewClient("http://example.invalid", "")}
	if got := b2.doReload(context.Background()); !strings.Contains(got, "控制未启用") {
		t.Fatalf("empty-token doReload = %q", got)
	}
}

func TestCallbackAuthorizationUsesOriginalChat(t *testing.T) {
	const groupID int64 = -100123
	b := &Bot{chatID: groupID}

	if !b.chatAllowed(callbackContext(groupID, 42, 9, "reload:cancel")) {
		t.Fatal("callback from configured group should be allowed regardless of clicking user")
	}
	if b.chatAllowed(callbackContext(-100999, groupID, 9, "reload:cancel")) {
		t.Fatal("callback from another chat must not be authorized by clicking user ID")
	}

	inaccessible := &ext.Context{Update: &ext.Update{CallbackQuery: &tg.CallbackQuery{
		ID:      "inaccessible",
		Message: &tg.InaccessibleMessage{Chat: &tg.Chat{ID: groupID}, MessageID: 10},
		Data:    "reload:cancel",
	}}}
	if !b.chatAllowed(inaccessible) {
		t.Fatal("inaccessible callback message should retain chat authorization")
	}

	inline := &ext.Context{Update: &ext.Update{CallbackQuery: &tg.CallbackQuery{
		ID:              "inline",
		From:            &tg.User{ID: groupID},
		InlineMessageID: "inline-message",
		Data:            "reload:cancel",
	}}}
	if b.chatAllowed(inline) {
		t.Fatal("inline callback without a chat target must not be authorized")
	}
}

func TestHandleCallbackRepliesToGroupAndClearsKeyboard(t *testing.T) {
	const groupID int64 = -100123
	recorder := &telegramRecorder{}
	srv := newFakeTelegramServer(t, recorder)
	defer srv.Close()

	b := &Bot{bot: newTestTelegramBot(t, srv.URL), chatID: groupID}
	if err := b.handleCallback(context.Background(), callbackContext(groupID, 42, 9, "reload:cancel")); err != nil {
		t.Fatalf("handleCallback: %v", err)
	}

	sends := recorder.callsFor("sendMessage")
	if len(sends) != 1 || int64(sends[0].body["chat_id"].(float64)) != groupID {
		t.Fatalf("sendMessage calls = %#v", sends)
	}
	if got := int64(sends[0].body["message_thread_id"].(float64)); got != 17 {
		t.Fatalf("message_thread_id = %d, want 17", got)
	}
	edits := recorder.callsFor("editMessageReplyMarkup")
	if len(edits) != 1 || int64(edits[0].body["chat_id"].(float64)) != groupID {
		t.Fatalf("editMessageReplyMarkup calls = %#v", edits)
	}
	if answers := recorder.callsFor("answerCallbackQuery"); len(answers) != 1 {
		t.Fatalf("answerCallbackQuery calls = %#v", answers)
	}
}

func TestHandleInaccessibleCallbackRepliesToOriginalChat(t *testing.T) {
	const groupID int64 = -100456
	recorder := &telegramRecorder{}
	srv := newFakeTelegramServer(t, recorder)
	defer srv.Close()

	b := &Bot{bot: newTestTelegramBot(t, srv.URL), chatID: groupID}
	c := &ext.Context{Update: &ext.Update{CallbackQuery: &tg.CallbackQuery{
		ID:      "inaccessible",
		From:    &tg.User{ID: 99},
		Message: &tg.InaccessibleMessage{Chat: &tg.Chat{ID: groupID}, MessageID: 10},
		Data:    "reload:cancel",
	}}}
	if err := b.handleCallback(context.Background(), c); err != nil {
		t.Fatalf("handleCallback: %v", err)
	}
	sends := recorder.callsFor("sendMessage")
	if len(sends) != 1 || int64(sends[0].body["chat_id"].(float64)) != groupID {
		t.Fatalf("sendMessage calls = %#v", sends)
	}
	if edits := recorder.callsFor("editMessageReplyMarkup"); len(edits) != 0 {
		t.Fatalf("inaccessible message must not be edited: %#v", edits)
	}
}

func TestHandleInlineCallbackRejectsWithoutSending(t *testing.T) {
	recorder := &telegramRecorder{}
	srv := newFakeTelegramServer(t, recorder)
	defer srv.Close()

	b := &Bot{bot: newTestTelegramBot(t, srv.URL), chatID: 42}
	c := &ext.Context{Update: &ext.Update{CallbackQuery: &tg.CallbackQuery{
		ID:              "inline",
		From:            &tg.User{ID: 42},
		InlineMessageID: "inline-message",
		Data:            "reload:cancel",
	}}}
	if err := b.handleCallback(context.Background(), c); err != nil {
		t.Fatalf("handleCallback: %v", err)
	}
	if sends := recorder.callsFor("sendMessage"); len(sends) != 0 {
		t.Fatalf("unexpected sendMessage calls: %#v", sends)
	}
	if edits := recorder.callsFor("editMessageReplyMarkup"); len(edits) != 0 {
		t.Fatalf("unexpected editMessageReplyMarkup calls: %#v", edits)
	}
	answers := recorder.callsFor("answerCallbackQuery")
	if len(answers) != 1 || answers[0].body["show_alert"] != true {
		t.Fatalf("answerCallbackQuery calls = %#v", answers)
	}
}

func TestToggleLongRuleIDCallbackIsBoundedAndIdempotent(t *testing.T) {
	const chatID int64 = 42
	ruleID := strings.Repeat("very-long-rule-id-", 8)
	f := &fakeControl{
		rules:    []engine.Rule{{RuleID: ruleID, Enabled: true}},
		revision: "sha256:abc",
	}
	controlServer := newFakeControlServer(t, f)
	defer controlServer.Close()

	recorder := &telegramRecorder{}
	telegramServer := newFakeTelegramServer(t, recorder)
	defer telegramServer.Close()

	b := &Bot{
		bot:     newTestTelegramBot(t, telegramServer.URL),
		control: controlapi.NewClient(controlServer.URL, "tok"),
		chatID:  chatID,
	}
	if err := b.handleToggle(context.Background(), commandContext(chatID, "/toggle "+ruleID)); err != nil {
		t.Fatalf("handleToggle: %v", err)
	}
	sends := recorder.callsFor("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage count = %d, want 1", len(sends))
	}
	callbackData := callbackDataFromSend(t, sends[0])
	if len(callbackData) > callbackDataByteLimit {
		t.Fatalf("callback_data is %d bytes: %q", len(callbackData), callbackData)
	}
	if strings.Contains(callbackData, ruleID) {
		t.Fatalf("callback_data leaked long rule ID: %q", callbackData)
	}
	enabled, _, ok := parseRuleSetCallbackData(callbackData)
	if !ok || enabled {
		t.Fatalf("parsed callback = enabled:%v ok:%v, want target disabled", enabled, ok)
	}

	callback := callbackContext(chatID, 9001, 11, callbackData)
	if err := b.handleCallback(context.Background(), callback); err != nil {
		t.Fatalf("first handleCallback: %v", err)
	}
	if err := b.handleCallback(context.Background(), callback); err != nil {
		t.Fatalf("second handleCallback: %v", err)
	}
	if f.putCount != 1 {
		t.Fatalf("PUT count = %d, want 1 after duplicate callback", f.putCount)
	}
	if f.lastPut == nil || len(f.lastPut.Rules) != 1 || f.lastPut.Rules[0].RuleID != ruleID || f.lastPut.Rules[0].Enabled {
		t.Fatalf("PUT body = %+v", f.lastPut)
	}
}

func TestRuleCallbackTokensDoNotTruncateCommonPrefix(t *testing.T) {
	prefix := strings.Repeat("same-prefix-", 10)
	first := ruleSetCallbackData(prefix+"first", true)
	second := ruleSetCallbackData(prefix+"second", true)
	if first == second {
		t.Fatalf("distinct rule IDs mapped to the same callback: %q", first)
	}
	if len(first) > callbackDataByteLimit || len(second) > callbackDataByteLimit {
		t.Fatalf("callback data lengths = %d, %d", len(first), len(second))
	}
}

func TestBuildRuleMessagesSplitsAtTelegramLimit(t *testing.T) {
	rules := make([]engine.Rule, 0, 300)
	stats := make(map[string]engine.TrafficSnapshot, 300)
	for i := 0; i < 300; i++ {
		ruleID := fmt.Sprintf("rule-%03d", i)
		rules = append(rules, engine.Rule{
			RuleID:     ruleID,
			Name:       fmt.Sprintf("long-rule-name-%03d", i),
			Protocol:   engine.ProtocolTCP,
			ListenAddr: "2001:db8::1234",
			ListenPort: 10000 + i,
			TargetAddr: "target.example.internal",
			TargetPort: 20000 + i,
		})
		stats[ruleID] = engine.TrafficSnapshot{RuleID: ruleID, Conns: int64(i), UploadBytes: int64(i) << 20}
	}

	chunks := buildRuleMessages(rules, stats)
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > telegramTextLimit {
			t.Fatalf("chunk %d is %d bytes", i, len(chunk))
		}
		if strings.Contains(chunk, "```") {
			t.Fatalf("chunk %d unexpectedly uses Markdown fencing", i)
		}
		if !strings.HasPrefix(chunk, "Forwarding Rules\n") {
			t.Fatalf("chunk %d missing repeated header", i)
		}
	}
}

func TestSendRuleMessagesReturnsTelegramError(t *testing.T) {
	recorder := &telegramRecorder{failMethod: "sendMessage"}
	srv := newFakeTelegramServer(t, recorder)
	defer srv.Close()

	b := &Bot{bot: newTestTelegramBot(t, srv.URL)}
	err := b.sendRuleMessages(context.Background(), 42, []string{"Forwarding Rules"})
	if err == nil || !strings.Contains(err.Error(), "send rules chunk 1/1") {
		t.Fatalf("sendRuleMessages error = %v", err)
	}
}
