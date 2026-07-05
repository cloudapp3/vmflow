package acme

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

const defaultExecTimeout = 60 * time.Second

// ExecProvider manages DNS TXT records by calling an external script.
//
// The script is called with:
//
//	present <fqdn> <value>   — create the TXT record
//	cleanup <fqdn>           — remove the TXT record
//
// Exit code 0 = success, non-zero = failure.
// stdout/stderr are captured for error reporting.
type ExecProvider struct {
	path    string
	timeout time.Duration
}

// NewExecProvider creates a provider that delegates to an external script.
func NewExecProvider(path string) *ExecProvider {
	return &ExecProvider{
		path:    path,
		timeout: defaultExecTimeout,
	}
}

// Present calls the script with "present <fqdn> <value>".
func (p *ExecProvider) Present(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, "present", fqdn, value)
}

// CleanUp calls the script with "cleanup <fqdn>".
func (p *ExecProvider) CleanUp(ctx context.Context, fqdn string) error {
	return p.run(ctx, "cleanup", fqdn)
}

func (p *ExecProvider) run(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec dns provider %s %v: %w\noutput: %s", p.path, args, err, string(out))
	}
	return nil
}
