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

const serverWriteInstructions = "vmflow daemon inspection and forwarding-rule configuration. Read tools behave as in read-only mode. Write tools mutate one rule per call, revalidate through daemon-side precheck, and use optimistic concurrency; on a revision conflict, re-read the configuration and retry. Set dry_run=true to validate a change without applying it. Forwarding listeners on non-loopback addresses require allow_non_loopback=true. An admin management token is required for writes."

// Options configures optional MCP server capabilities. WriteEnabled is off by
// default; enabling it registers the single-rule write tools when the
// management client also implements WriteManagementClient.
type Options struct {
	WriteEnabled bool
}

// NewServer builds a tools-only MCP server backed by vmflow's daemon
// management API.
func NewServer(client ManagementClient, version string, opts Options) *mcp.Server {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	instructions := serverInstructions
	var write writeBackend
	if opts.WriteEnabled {
		instructions = serverWriteInstructions
		if writeClient, ok := client.(WriteManagementClient); ok {
			write = managementWriteBackend{write: writeClient}
		}
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "vmflow",
		Title:   "vmflow forwarding diagnostics",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{},
	})
	registerTools(server, newManagementBackend(client), write, version)
	return server
}

// RunStdio serves one MCP client over the supplied streams. The streams are
// not closed because the caller normally owns os.Stdin and os.Stdout. Callers
// must keep diagnostics on stderr so stdout remains a JSON-RPC-only channel.
func RunStdio(ctx context.Context, stdin io.Reader, stdout io.Writer, client ManagementClient, version string, opts Options) error {
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
	if err := NewServer(client, version, opts).Run(ctx, transport); err != nil {
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
