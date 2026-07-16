package controlapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientCurrentPrecheckDecodesStructuredResults(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantOK         bool
		wantError      string
		wantCheck      string
		wantRuleCount  int
		wantConfigPath string
	}{
		{
			name:           "ok",
			status:         http.StatusOK,
			body:           `{"config_path":"config.yaml","rule_count":2,"result":{"ok":true,"error_count":0,"warning_count":0,"checked_rules":2,"checked_time_ms":1,"items":[]}}`,
			wantOK:         true,
			wantRuleCount:  2,
			wantConfigPath: "config.yaml",
		},
		{
			name:           "validation findings",
			status:         http.StatusBadRequest,
			body:           `{"config_path":"config.yaml","rule_count":1,"result":{"ok":false,"error_count":1,"warning_count":0,"checked_rules":1,"checked_time_ms":1,"items":[{"severity":"error","check":"listen_bind","rule_id":"rule-1","message":"address already in use"}]}}`,
			wantCheck:      "listen_bind",
			wantRuleCount:  1,
			wantConfigPath: "config.yaml",
		},
		{
			name:      "configuration load failure",
			status:    http.StatusBadRequest,
			body:      `{"ok":false,"error":"configuration could not be loaded","result":{"ok":false,"error_count":1,"warning_count":0,"checked_rules":0,"checked_time_ms":0,"items":[{"severity":"error","check":"config_load","message":"configuration could not be loaded"}]}}`,
			wantError: "configuration could not be loaded",
			wantCheck: "config_load",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := clientWithResponse(t, test.status, test.body, func(request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/v1/precheck" {
					t.Fatalf("request = %s %s, want GET /v1/precheck", request.Method, request.URL.Path)
				}
			})
			response, err := client.CurrentPrecheck(context.Background())
			if err != nil {
				t.Fatalf("CurrentPrecheck() error = %v", err)
			}
			if response.Result.OK != test.wantOK || response.Error != test.wantError {
				t.Fatalf("CurrentPrecheck() = %+v", response)
			}
			if response.RuleCount != test.wantRuleCount || response.ConfigPath != test.wantConfigPath {
				t.Fatalf("CurrentPrecheck() metadata = (%q, %d)", response.ConfigPath, response.RuleCount)
			}
			if test.wantCheck != "" {
				if len(response.Result.Items) != 1 || response.Result.Items[0].Check != test.wantCheck {
					t.Fatalf("CurrentPrecheck() findings = %+v", response.Result.Items)
				}
			}
		})
	}
}

func TestClientCurrentPrecheckRejectsOtherStatuses(t *testing.T) {
	client := clientWithResponse(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, nil)
	response, err := client.CurrentPrecheck(context.Background())
	if response != nil {
		t.Fatalf("CurrentPrecheck() response = %+v, want nil", response)
	}
	if APIStatus(err) != http.StatusUnauthorized {
		t.Fatalf("CurrentPrecheck() status = %d, want 401; err=%v", APIStatus(err), err)
	}
}

func TestClientSessionDecodesLegacyPayload(t *testing.T) {
	client := clientWithResponse(t, http.StatusOK, `{"actor":"operator","role":"viewer","capabilities":{"rules_write":false}}`, nil)
	response, err := client.Session(context.Background())
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	if response.Actor != "operator" || response.Role != RoleViewer {
		t.Fatalf("Session() identity = %+v", response)
	}
	if response.APIVersion != "" || response.ServerVersion != "" || response.Commit != "" || response.StartedTime != 0 || response.Degraded || response.DegradedCause != "" {
		t.Fatalf("legacy Session() metadata = %+v, want zero values", response)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func clientWithResponse(t *testing.T, status int, body string, inspect func(*http.Request)) *Client {
	t.Helper()
	client := NewClient("http://vmflow.test", "")
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(request)
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})})
	return client
}
