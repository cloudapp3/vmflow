package bot

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	tg "github.com/cloudapp3/tgbot"
	"github.com/cloudapp3/vmflow/controlapi"
)

func TestBotHandlerErrorSummaryRedactsTelegramNetworkURL(t *testing.T) {
	const secret = "123456:super-secret-token"
	err := fmt.Errorf("send telegram request: %w", &url.Error{
		Op:  http.MethodPost,
		URL: "https://api.telegram.org/bot" + secret + "/sendMessage",
		Err: syscall.ECONNREFUSED,
	})

	summary := botHandlerErrorSummary(err)
	if !strings.Contains(summary, "category=network") {
		t.Fatalf("summary = %q", summary)
	}
	if strings.Contains(summary, secret) || strings.Contains(summary, "api.telegram.org") || strings.Contains(summary, err.Error()) {
		t.Fatalf("summary leaked raw Telegram error: %q", summary)
	}
}

func TestBotHandlerErrorSummaryUsesSafeStructuredFields(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "telegram api",
			err: &tg.APIError{
				StatusCode: http.StatusTooManyRequests,
				Code:       http.StatusTooManyRequests,
				Message:    "secret response detail",
				RetryAfter: 12,
			},
			want: "category=telegram_api status=429 code=429 retry_after=12",
		},
		{
			name: "control api",
			err: &controlapi.APIError{
				StatusCode: http.StatusForbidden,
				Message:    "secret response detail",
				Body:       []byte("secret body"),
			},
			want: "category=control_api status=403",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := botHandlerErrorSummary(test.err); got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}
