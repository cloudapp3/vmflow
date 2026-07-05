package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ── API Response Types ─────────────────────────────────────────────

type HealthResponse struct {
	OK           bool  `json:"ok"`
	RunningRules int   `json:"running_rules"`
	Time         int64 `json:"time"`
}

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

type RulesResponse struct {
	Items []RuleInfo `json:"items"`
}

type RuleInfo struct {
	RuleID     string `json:"rule_id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	ListenAddr string `json:"listen_addr"`
	ListenPort int    `json:"listen_port"`
	TargetAddr string `json:"target_addr"`
	TargetPort int    `json:"target_port"`
	Enabled    bool   `json:"enabled"`
	SpeedLimit int64  `json:"speed_limit"`
	MaxConn    int    `json:"max_conn"`
	Remark     string `json:"remark,omitempty"`
}

type ReloadResponse struct {
	ConfigPath      string `json:"config_path"`
	AdminListenAddr string `json:"admin_listen_addr"`
	RuleCount       int    `json:"rule_count"`
}

// ── Client ─────────────────────────────────────────────────────────

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL string, token ...string) *Client {
	client := &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
	if len(token) > 0 {
		client.token = token[0]
	}
	return client
}

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	resp := new(HealthResponse)
	if err := c.doGet(ctx, "/healthz", resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Stats(ctx context.Context) (*StatsResponse, error) {
	resp := new(StatsResponse)
	if err := c.doGet(ctx, "/v1/stats", resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Rules(ctx context.Context) (*RulesResponse, error) {
	resp := new(RulesResponse)
	if err := c.doGet(ctx, "/v1/rules", resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Reload(ctx context.Context) (*ReloadResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/reload", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reload failed: %s", body)
	}
	result := new(ReloadResponse)
	if err := json.Unmarshal(body, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) doGet(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, body)
	}
	return json.Unmarshal(body, result)
}

func (c *Client) authorize(req *http.Request) {
	if c != nil && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
