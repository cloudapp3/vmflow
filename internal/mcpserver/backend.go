package mcpserver

import (
	"context"
	"fmt"

	"github.com/cloudapp3/vmflow/controlapi"
)

// ManagementClient is the read-only subset of the daemon management client
// used by MCP. Keeping this boundary small makes tool contracts testable and
// prevents write APIs from becoming reachable through the MCP server.
type ManagementClient interface {
	Session(context.Context) (*controlapi.SessionResponse, error)
	ConfigRules(context.Context) (*controlapi.ConfigRulesResponse, error)
	Rules(context.Context) (*controlapi.RulesResponse, error)
	Stats(context.Context) (*controlapi.StatsResponse, error)
	CurrentPrecheck(context.Context) (*controlapi.CurrentPrecheckResponse, error)
}

type backend interface {
	Session(context.Context) (*controlapi.SessionResponse, error)
	ConfigRules(context.Context) (*controlapi.ConfigRulesResponse, error)
	Rules(context.Context) (*controlapi.RulesResponse, error)
	Stats(context.Context) (*controlapi.StatsResponse, error)
	CurrentPrecheck(context.Context) (*controlapi.CurrentPrecheckResponse, error)
}

// managementBackend is deliberately just an adapter around ManagementClient.
// The adapter is the only production backend: MCP never talks to engine.Manager
// directly and therefore cannot create a second forwarding runtime.
type managementBackend struct {
	client ManagementClient
}

func newManagementBackend(client ManagementClient) backend {
	return managementBackend{client: client}
}

func (b managementBackend) Session(ctx context.Context) (*controlapi.SessionResponse, error) {
	if b.client == nil {
		return nil, missingManagementClient()
	}
	return b.client.Session(ctx)
}

func (b managementBackend) ConfigRules(ctx context.Context) (*controlapi.ConfigRulesResponse, error) {
	if b.client == nil {
		return nil, missingManagementClient()
	}
	return b.client.ConfigRules(ctx)
}

func (b managementBackend) Rules(ctx context.Context) (*controlapi.RulesResponse, error) {
	if b.client == nil {
		return nil, missingManagementClient()
	}
	return b.client.Rules(ctx)
}

func (b managementBackend) Stats(ctx context.Context) (*controlapi.StatsResponse, error) {
	if b.client == nil {
		return nil, missingManagementClient()
	}
	return b.client.Stats(ctx)
}

func (b managementBackend) CurrentPrecheck(ctx context.Context) (*controlapi.CurrentPrecheckResponse, error) {
	if b.client == nil {
		return nil, missingManagementClient()
	}
	return b.client.CurrentPrecheck(ctx)
}

func missingManagementClient() error {
	return fmt.Errorf("vmflow management client is required")
}
