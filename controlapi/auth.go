package controlapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/cloudapp3/vmflow/config"
)

type contextKey string

const authInfoContextKey contextKey = "auth_info"

const (
	RoleAdmin  = config.AuthRoleAdmin
	RoleViewer = config.AuthRoleViewer
)

// AuthInfo describes the authenticated Admin API caller.
type AuthInfo struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type authRule struct {
	name  string
	token string
	role  string
}

// Authenticator validates Admin API bearer tokens.
type Authenticator struct {
	enabled bool
	rules   []authRule
}

// NewAuthenticator builds an authenticator from config.
func NewAuthenticator(cfg config.AuthConfig) *Authenticator {
	a := &Authenticator{enabled: cfg.Enabled}
	for _, item := range cfg.Tokens {
		if strings.TrimSpace(item.Token) == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role == "" {
			role = RoleAdmin
		}
		a.rules = append(a.rules, authRule{
			name:  strings.TrimSpace(item.Name),
			token: strings.TrimSpace(item.Token),
			role:  role,
		})
	}
	return a
}

// Enabled returns whether authentication is enforced.
func (a *Authenticator) Enabled() bool {
	return a != nil && a.enabled
}

// Authenticate validates the Authorization header and returns caller info.
func (a *Authenticator) Authenticate(r *http.Request) (AuthInfo, bool) {
	if !a.Enabled() {
		return AuthInfo{Name: "anonymous", Role: RoleAdmin}, true
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return AuthInfo{}, false
	}
	provided := strings.TrimSpace(value[len(prefix):])
	if provided == "" {
		return AuthInfo{}, false
	}
	for _, rule := range a.rules {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(rule.token)) == 1 {
			name := rule.name
			if name == "" {
				name = "token"
			}
			role := rule.role
			if role == "" {
				role = RoleAdmin
			}
			return AuthInfo{Name: name, Role: role}, true
		}
	}
	return AuthInfo{}, false
}

func (info AuthInfo) canWrite() bool {
	return info.Role == RoleAdmin
}

func withAuthInfo(ctx context.Context, info AuthInfo) context.Context {
	return context.WithValue(ctx, authInfoContextKey, info)
}

// AuthInfoFromContext returns caller auth info from a request context.
func AuthInfoFromContext(ctx context.Context) (AuthInfo, bool) {
	info, ok := ctx.Value(authInfoContextKey).(AuthInfo)
	return info, ok
}
