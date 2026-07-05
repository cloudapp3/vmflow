package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const cloudflareBaseURL = "https://api.cloudflare.com/client/v4"

// CloudflareProvider manages DNS TXT records via the Cloudflare API.
type CloudflareProvider struct {
	apiToken   string
	httpClient *http.Client
}

// NewCloudflareProvider creates a provider using a Cloudflare API token.
// The token needs Zone:DNS:Edit and Zone:Zone:Read permissions.
func NewCloudflareProvider(apiToken string) *CloudflareProvider {
	return &CloudflareProvider{
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// cfResponse is the generic Cloudflare API response envelope.
type cfResponse struct {
	Success  bool            `json:"success"`
	Errors   []cfError       `json:"errors"`
	Messages []string        `json:"messages"`
	Result   json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"` // 1 = automatic
	Proxied bool   `json:"proxied"`
}

// Present creates a TXT record for the ACME dns-01 challenge.
func (p *CloudflareProvider) Present(ctx context.Context, fqdn, value string) error {
	zoneID, err := p.findZoneID(ctx, fqdn)
	if err != nil {
		return fmt.Errorf("cloudflare: find zone: %w", err)
	}

	record := cfDNSRecord{
		Type:    "TXT",
		Name:    fqdn,
		Content: value,
		TTL:     1, // automatic
		Proxied: false,
	}

	body, err := json.Marshal(record)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records", cloudflareBaseURL, zoneID)
	resp, err := p.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("cloudflare: create record: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("cloudflare: create record failed: %s", cfErrorMessages(resp.Errors))
	}

	return nil
}

// CleanUp removes the TXT record previously created by Present.
func (p *CloudflareProvider) CleanUp(ctx context.Context, fqdn string) error {
	zoneID, err := p.findZoneID(ctx, fqdn)
	if err != nil {
		return fmt.Errorf("cloudflare: find zone: %w", err)
	}

	recordID, err := p.findRecordID(ctx, zoneID, fqdn)
	if err != nil {
		return fmt.Errorf("cloudflare: find record: %w", err)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cloudflareBaseURL, zoneID, recordID)
	resp, err := p.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("cloudflare: delete record: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("cloudflare: delete record failed: %s", cfErrorMessages(resp.Errors))
	}

	return nil
}

func (p *CloudflareProvider) findZoneID(ctx context.Context, fqdn string) (string, error) {
	// Extract the base domain from the FQDN.
	// _acme-challenge.sub.example.com -> try sub.example.com, then example.com
	domain := strings.TrimPrefix(fqdn, "_acme-challenge.")
	domain = strings.TrimSuffix(domain, ".")

	parts := strings.Split(domain, ".")
	for i := 0; i < len(parts)-1; i++ {
		zoneName := strings.Join(parts[i:], ".")
		url := fmt.Sprintf("%s/zones?name=%s&status=active", cloudflareBaseURL, zoneName)

		resp, err := p.doRequest(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}

		var zones []cfZone
		if err := json.Unmarshal(resp.Result, &zones); err != nil {
			continue
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}

	return "", fmt.Errorf("no active Cloudflare zone found for %s", fqdn)
}

func (p *CloudflareProvider) findRecordID(ctx context.Context, zoneID, fqdn string) (string, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records?type=TXT&name=%s", cloudflareBaseURL, zoneID, fqdn)

	resp, err := p.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	var records []cfDNSRecord
	if err := json.Unmarshal(resp.Result, &records); err != nil {
		return "", fmt.Errorf("unmarshal records: %w", err)
	}

	if len(records) == 0 {
		return "", fmt.Errorf("no TXT record found for %s", fqdn)
	}

	return records[0].ID, nil
}

func (p *CloudflareProvider) doRequest(ctx context.Context, method, url string, body []byte) (*cfResponse, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiToken)
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, string(respBody))
	}

	var cfResp cfResponse
	if err := json.Unmarshal(respBody, &cfResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return &cfResp, nil
}

func cfErrorMessages(errs []cfError) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, fmt.Sprintf("%d: %s", e.Code, e.Message))
	}
	return strings.Join(msgs, "; ")
}
