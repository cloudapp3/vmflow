package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/mcpserver"
)

type mcpOptions struct {
	addr    string
	token   string
	tls     controlapi.ClientTLSOptions
	headers controlapi.HeaderFlags
}

func runMCP(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runMCPWithIO(ctx, args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "vmflow MCP server failed: %v\n", err)
		os.Exit(1)
	}
}

func runMCPWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	opts, err := parseMCPOptions(args, stderr)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 35 * time.Second}
	if opts.tls.Any() {
		httpClient, err = controlapi.NewHTTPClient(opts.tls, 35*time.Second)
		if err != nil {
			return fmt.Errorf("configure daemon management TLS: %w", err)
		}
	}
	client := controlapi.NewClient(opts.addr, opts.token)
	client.SetHTTPClient(httpClient)
	client.SetHeaders(opts.headers.HTTPHeader())

	if err := mcpserver.RunStdio(ctx, stdin, stdout, client, version); err != nil {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}

func parseMCPOptions(args []string, output io.Writer) (mcpOptions, error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(output)
	defaults := loadManagementDefaults(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage:\n  vmflow mcp [flags]\n\nStarts a read-only MCP server over stdio for a running local vmflow daemon.\n\nOptions:")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", defaults.Address, "local daemon management address")
	token := fs.String("token", defaults.Token, "daemon management token (or environment/client profile)")
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	if err := fs.Parse(args); err != nil {
		return mcpOptions{}, err
	}
	if extra := fs.Args(); len(extra) != 0 {
		return mcpOptions{}, fmt.Errorf("unexpected argument(s): %v", extra)
	}

	address, err := normalizeMCPControlAddress(*addr)
	if err != nil {
		return mcpOptions{}, err
	}
	tlsOpts := tlsFlags.Opts()
	parsed, _ := url.Parse(address)
	if tlsOpts.Any() && parsed.Scheme != "https" {
		return mcpOptions{}, fmt.Errorf("MCP TLS options require an https daemon management address")
	}
	return mcpOptions{
		addr:    address,
		token:   strings.TrimSpace(*token),
		tls:     tlsOpts,
		headers: *headerFlags,
	}, nil
}

func normalizeMCPControlAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid daemon management address: %w", err)
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("daemon management address must use http or https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("daemon management address must not contain credentials")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("daemon management address requires a host")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("daemon management address must use localhost or a loopback IP")
		}
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("daemon management address must not contain a path")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("daemon management address must not contain a query or fragment")
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
