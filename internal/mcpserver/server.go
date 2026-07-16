package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = "Read-only vmflow daemon inspection. Rule lists omit listen and target addresses, source IPs, and domains; request one rule explicitly to inspect its full configuration. Configuration precheck may resolve configured network targets but never changes daemon state."

// NewServer builds a tools-only MCP server backed by vmflow's daemon
// management API.
func NewServer(client ManagementClient, version string) *mcp.Server {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vmflow",
		Title:   "vmflow forwarding diagnostics",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Capabilities: &mcp.ServerCapabilities{},
	})
	registerTools(server, newManagementBackend(client), version)
	return server
}

// RunStdio serves one MCP client over the supplied streams. The streams are
// not closed because the caller normally owns os.Stdin and os.Stdout. Callers
// must keep diagnostics on stderr so stdout remains a JSON-RPC-only channel.
func RunStdio(ctx context.Context, stdin io.Reader, stdout io.Writer, client ManagementClient, version string) error {
	if stdin == nil {
		return fmt.Errorf("MCP stdin is required")
	}
	if stdout == nil {
		return fmt.Errorf("MCP stdout is required")
	}
	transport := &mcp.IOTransport{
		Reader: io.NopCloser(stdin),
		Writer: nopWriteCloser{Writer: stdout},
	}
	if err := NewServer(client, version).Run(ctx, transport); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
