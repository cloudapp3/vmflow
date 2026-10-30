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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/mcpserver"
)

type mcpOptions struct {
	addr    string
	token   string
	write   bool
	tls     controlapi.ClientTLSOptions
	headers controlapi.HeaderFlags
}

func runMCP(args []string) {
	runMCPWithTranslator(args, defaultTranslator())
}

func runMCPWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runMCPWithIOAndTranslator(ctx, args, os.Stdin, os.Stdout, os.Stderr, translator); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, translator.Tf("cli.mcp.error.failed", err))
		os.Exit(1)
	}
}

func runMCPWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runMCPWithIOAndTranslator(ctx, args, stdin, stdout, stderr, defaultTranslator())
}

func runMCPWithIOAndTranslator(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, translator *i18n.Translator) error {
	translator = translatorOrEnglish(translator)
	opts, err := parseMCPOptionsWithTranslator(args, stderr, translator)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 35 * time.Second}
	if opts.tls.Any() {
		httpClient, err = controlapi.NewHTTPClient(opts.tls, 35*time.Second)
		if err != nil {
			return fmt.Errorf("%s", translator.Tf("cli.mcp.error.configure_tls", err))
		}
	}
	client := controlapi.NewClient(opts.addr, opts.token)
	client.SetHTTPClient(httpClient)
	client.SetHeaders(opts.headers.HTTPHeader())

	serverOptions := mcpserver.Options{WriteEnabled: opts.write}
	if err := mcpserver.RunStdio(ctx, stdin, stdout, client, version, serverOptions); err != nil {
		return fmt.Errorf("%s", translator.Tf("cli.mcp.error.serve_stdio", err))
	}
	return nil
}

func parseMCPOptions(args []string, output io.Writer) (mcpOptions, error) {
	return parseMCPOptionsWithTranslator(args, output, defaultTranslator())
}

func parseMCPOptionsWithTranslator(args []string, output io.Writer, translator *i18n.Translator) (mcpOptions, error) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(output)
	defaults := loadManagementDefaultsWithTranslator(output, translator)
	fs.Usage = func() {
		fmt.Fprintln(output, translator.T("cli.mcp.usage"))
		fs.PrintDefaults()
	}
	addr := fs.String("addr", defaults.Address, translator.T("cli.mcp.flag.address"))
	token := fs.String("token", defaults.Token, translator.T("cli.mcp.flag.token"))
	writeDefault, err := mcpWriteEnvDefault(os.Getenv("VMFLOW_MCP_WRITE"))
	if err != nil {
		return mcpOptions{}, fmt.Errorf("%s", translator.Tf("cli.mcp.error.write_env", err))
	}
	write := fs.Bool("write", writeDefault, translator.T("cli.mcp.flag.write"))
	tlsFlags := controlapi.AddClientTLSFlags(fs)
	headerFlags := controlapi.AddHeaderFlags(fs)
	localizeControlClientFlags(fs, translator)
	if err := fs.Parse(args); err != nil {
		return mcpOptions{}, err
	}
	if extra := fs.Args(); len(extra) != 0 {
		return mcpOptions{}, fmt.Errorf("%s", translator.Tf("cli.error.unexpected_arguments", extra))
	}

	address, err := normalizeMCPControlAddressWithTranslator(*addr, translator)
	if err != nil {
		return mcpOptions{}, err
	}
	tlsOpts := tlsFlags.Opts()
	parsed, _ := url.Parse(address)
	if tlsOpts.Any() && parsed.Scheme != "https" {
		return mcpOptions{}, fmt.Errorf("%s", translator.T("cli.mcp.error.tls_requires_https"))
	}
	return mcpOptions{
		addr:    address,
		token:   strings.TrimSpace(*token),
		write:   *write,
		tls:     tlsOpts,
		headers: *headerFlags,
	}, nil
}

// mcpWriteEnvDefault parses VMFLOW_MCP_WRITE. An unset or empty value keeps
// read-only mode; an unparsable value is an error rather than a silent no-op.
func mcpWriteEnvDefault(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("VMFLOW_MCP_WRITE must be a boolean value, got %q", raw)
	}
	return value, nil
}

func normalizeMCPControlAddress(raw string) (string, error) {
	return normalizeMCPControlAddressWithTranslator(raw, defaultTranslator())
}

func normalizeMCPControlAddressWithTranslator(raw string, translator *i18n.Translator) (string, error) {
	translator = translatorOrEnglish(translator)
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s", translator.Tf("cli.mcp.error.invalid_address", err))
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s", translator.T("cli.mcp.error.scheme"))
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%s", translator.T("cli.mcp.error.credentials"))
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("%s", translator.T("cli.mcp.error.host"))
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("%s", translator.T("cli.mcp.error.loopback"))
		}
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%s", translator.T("cli.mcp.error.path"))
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s", translator.T("cli.mcp.error.query"))
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
