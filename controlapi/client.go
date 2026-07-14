package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/engine"
	"github.com/cloudapp3/vmflow/precheck"
)

// API response types.

type StatsResponse struct {
	Items []TrafficSnapshot `json:"items"`
}

type TrafficSnapshot struct {
	RuleID        string `json:"rule_id"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
	Conns         int64  `json:"conns"`
	UpdatedTime   int64  `json:"updated_time"`
}

// RulesResponse is the legacy runtime-only rule endpoint. ConfigRulesResponse
// is the source of truth for management because it also contains disabled
// rules.
type RulesResponse struct {
	Items []engine.Rule `json:"items"`
}

type ReloadResponse struct {
	ConfigPath        string `json:"config_path"`
	ControlListenAddr string `json:"control_listen_addr"`
	RuleCount         int    `json:"rule_count"`
}

type SessionCapabilities struct {
	RulesWrite bool `json:"rules_write"`
}

type SessionResponse struct {
	Actor        string              `json:"actor"`
	Role         string              `json:"role"`
	Capabilities SessionCapabilities `json:"capabilities"`
}

// ConfigRulesResponse is the GET /v1/config/rules payload. ETag is populated
// from the response ETag header (or the revision) for optimistic concurrency.
type ConfigRulesResponse struct {
	Revision       string        `json:"revision"`
	Writable       bool          `json:"writable"`
	UDPMaxSessions int           `json:"udp_max_sessions"`
	Rules          []engine.Rule `json:"rules"`
	ETag           string        `json:"-"`
}

// ConfigRulesRequest is the PUT /v1/config/rules body.
type ConfigRulesRequest struct {
	UDPMaxSessions int           `json:"udp_max_sessions"`
	Rules          []engine.Rule `json:"rules"`
}

type ConfigRuleDiff struct {
	RuleID        string `json:"rule_id"`
	ConfigAction  string `json:"config_action"`
	RuntimeAction string `json:"runtime_action"`
}

type PrecheckResponse struct {
	Revision              string           `json:"revision"`
	UDPMaxSessionsChanged bool             `json:"udp_max_sessions_changed"`
	Diff                  []ConfigRuleDiff `json:"diff"`
	Precheck              precheck.Result  `json:"precheck"`
}

// ApplyResponse is the PUT /v1/config/rules success payload.
type ApplyResponse struct {
	Revision       string             `json:"revision"`
	Writable       bool               `json:"writable"`
	UDPMaxSessions int                `json:"udp_max_sessions"`
	Rules          []engine.Rule      `json:"rules"`
	Result         engine.ApplyResult `json:"result"`
	ETag           string             `json:"-"`
}

// Snapshot builds a ConfigRulesResponse from an apply result.
func (r *ApplyResponse) Snapshot() *ConfigRulesResponse {
	if r == nil {
		return nil
	}
	return &ConfigRulesResponse{
		Revision:       r.Revision,
		Writable:       r.Writable,
		UDPMaxSessions: r.UDPMaxSessions,
		Rules:          CloneRules(r.Rules),
		ETag:           r.ETag,
	}
}

// APIError preserves the HTTP status so callers can distinguish auth,
// validation, conflicts, and transient server failures.
type APIError struct {
	StatusCode int
	Message    string
	Body       []byte
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("control API returned HTTP %d", e.StatusCode)
}

// APIStatus returns the HTTP status embedded in err, or 0 if err is not an
// APIError.
func APIStatus(err error) int {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode
	}
	return 0
}

// Client calls one local or remote vmflow control API. Shared by the TUI,
// vmflow ctl, and the Telegram bot.
type Client struct {
	baseURL string
	token   string
	headers http.Header
	http    *http.Client
}

// NewClient returns a control API client for baseURL with the given bearer
// token. The token may be empty for read-only access.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// HasToken reports whether the client can perform authenticated write actions.
func (c *Client) HasToken() bool {
	return c != nil && c.token != ""
}

// SetHTTPClient replaces the HTTP client used for control API requests. Pass
// nil to keep the default; otherwise the caller controls TLS and timeouts.
func (c *Client) SetHTTPClient(h *http.Client) {
	if c != nil && h != nil {
		c.http = h
	}
}

// SetHeaders sets extra headers (for example Cloudflare Access credentials)
// applied to every request. nil clears them.
func (c *Client) SetHeaders(h http.Header) {
	if c != nil {
		c.headers = h
	}
}

// BaseURL returns the control API base URL the client was configured with.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

func (c *Client) Stats(ctx context.Context) (*StatsResponse, error) {
	resp := new(StatsResponse)
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/stats", "", nil, resp, http.StatusOK); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Rules(ctx context.Context) (*RulesResponse, error) {
	resp := new(RulesResponse)
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/rules", "", nil, resp, http.StatusOK); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Session(ctx context.Context) (*SessionResponse, error) {
	resp := new(SessionResponse)
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/session", "", nil, resp, http.StatusOK); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) ConfigRules(ctx context.Context) (*ConfigRulesResponse, error) {
	resp := new(ConfigRulesResponse)
	h, err := c.doJSON(ctx, http.MethodGet, "/v1/config/rules", "", nil, resp, http.StatusOK)
	if err != nil {
		return nil, err
	}
	resp.ETag = responseETag(h, resp.Revision)
	return resp, nil
}

// Precheck validates exactly the supplied draft. HTTP 422 is a successful
// protocol response carrying validation findings, not a transport error.
func (c *Client) Precheck(ctx context.Context, match string, draft ConfigRulesRequest) (*PrecheckResponse, error) {
	resp := new(PrecheckResponse)
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/config/rules/precheck", match, draft, resp,
		http.StatusOK, http.StatusUnprocessableEntity)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Apply replaces the managed rules configuration. match is the If-Match ETag
// from a prior ConfigRules call (optimistic concurrency).
func (c *Client) Apply(ctx context.Context, match string, draft ConfigRulesRequest) (*ApplyResponse, error) {
	resp := new(ApplyResponse)
	h, err := c.doJSON(ctx, http.MethodPut, "/v1/config/rules", match, draft, resp, http.StatusOK)
	if err != nil {
		return nil, err
	}
	resp.ETag = responseETag(h, resp.Revision)
	return resp, nil
}

// Reload reloads the daemon configuration from disk.
func (c *Client) Reload(ctx context.Context) (*ReloadResponse, error) {
	result := new(ReloadResponse)
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/reload", "", nil, result, http.StatusOK); err != nil {
		return nil, err
	}
	return result, nil
}

// BotConfigResponse is the GET /v1/config/bot payload. Tokens are plaintext
// (admin-only endpoint); the TUI masks them for display.
type BotConfigResponse struct {
	Revision        string `json:"revision"`
	BotToken        string `json:"bot_token"`
	BotChat         int64  `json:"bot_chat"`
	BotControlToken string `json:"bot_control_token"`
	Running         bool   `json:"running"`
	ETag            string `json:"-"`
}

// BotConfigRequest is the PUT /v1/config/bot body.
type BotConfigRequest struct {
	BotToken        string `json:"bot_token"`
	BotChat         int64  `json:"bot_chat"`
	BotControlToken string `json:"bot_control_token"`
}

// BotConfig fetches the current bot configuration and running state.
func (c *Client) BotConfig(ctx context.Context) (*BotConfigResponse, error) {
	resp := new(BotConfigResponse)
	h, err := c.doJSON(ctx, http.MethodGet, "/v1/config/bot", "", nil, resp, http.StatusOK)
	if err != nil {
		return nil, err
	}
	resp.ETag = responseETag(h, resp.Revision)
	return resp, nil
}

// ApplyBotConfig updates the bot configuration (persisted to config.yaml) and
// restarts the bot goroutine. match is the If-Match ETag from a prior BotConfig.
func (c *Client) ApplyBotConfig(ctx context.Context, match string, req BotConfigRequest) (*BotConfigResponse, error) {
	resp := new(BotConfigResponse)
	h, err := c.doJSON(ctx, http.MethodPut, "/v1/config/bot", match, req, resp, http.StatusOK)
	if err != nil {
		return nil, err
	}
	resp.ETag = responseETag(h, resp.Revision)
	return resp, nil
}

// StartBot starts the bot using the current persisted configuration.
func (c *Client) StartBot(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/bot/start", "", nil, nil, http.StatusOK)
	return err
}

// StopBot stops the running bot without changing persisted configuration.
func (c *Client) StopBot(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/bot/stop", "", nil, nil, http.StatusOK)
	return err
}

func (c *Client) doJSON(ctx context.Context, method, path, match string, requestBody, result any, accepted ...int) (http.Header, error) {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(match) != "" {
		req.Header.Set("If-Match", NormalizeETag(match))
	}
	c.authorize(req)
	c.applyExtraHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.Header, err
	}
	if !acceptedStatus(resp.StatusCode, accepted) {
		return resp.Header, decodeAPIError(resp.StatusCode, raw)
	}
	if result != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return resp.Header, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp.Header, nil
}

func (c *Client) authorize(req *http.Request) {
	if c != nil && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *Client) applyExtraHeaders(req *http.Request) {
	if c == nil || len(c.headers) == 0 {
		return
	}
	for name, values := range c.headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
}

func acceptedStatus(status int, accepted []int) bool {
	for _, candidate := range accepted {
		if status == candidate {
			return true
		}
	}
	return false
}

func decodeAPIError(status int, raw []byte) error {
	message := ""
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		message = strings.TrimSpace(payload.Error)
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: message, Body: append([]byte(nil), raw...)}
}

// NormalizeETag quotes a bare ETag value; already-quoted, weak, or wildcard
// values pass through unchanged.
func NormalizeETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "*" || strings.HasPrefix(value, "W/\"") || (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) {
		return value
	}
	return strconv.Quote(value)
}

func responseETag(header http.Header, revision string) string {
	if value := strings.TrimSpace(header.Get("ETag")); value != "" {
		return value
	}
	return NormalizeETag(revision)
}

// CloneRules deep-copies a rule slice including each rule's Domains.
func CloneRules(rules []engine.Rule) []engine.Rule {
	if rules == nil {
		return nil
	}
	result := make([]engine.Rule, len(rules))
	copy(result, rules)
	for index := range result {
		result[index].Domains = append([]string(nil), rules[index].Domains...)
	}
	return result
}
