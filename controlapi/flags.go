package controlapi

import (
	"flag"
	"net/http"
	"os"
	"strings"
)

// ClientTLSFlags holds the standard control-API client TLS flags registered
// on a *flag.FlagSet. Call Opts() after parsing to get a ClientTLSOptions.
type ClientTLSFlags struct {
	ca       *string
	cert     *string
	key      *string
	insecure *bool
}

// AddClientTLSFlags registers the standard control-API client TLS flags on fs
// (use flag.CommandLine for the legacy relay* binaries) and returns a handle.
// Each flag falls back to its VMFLOW_TLS_* environment variable.
func AddClientTLSFlags(fs *flag.FlagSet) *ClientTLSFlags {
	return &ClientTLSFlags{
		ca:       fs.String("tls-ca-file", os.Getenv("VMFLOW_TLS_CA_FILE"), "CA bundle to verify the control API server certificate (for private/self-signed CAs)"),
		cert:     fs.String("tls-client-cert", os.Getenv("VMFLOW_TLS_CLIENT_CERT"), "client certificate for mTLS (required when the server sets control_tls.client_ca_file)"),
		key:      fs.String("tls-client-key", os.Getenv("VMFLOW_TLS_CLIENT_KEY"), "client key for mTLS (required together with -tls-client-cert)"),
		insecure: fs.Bool("tls-skip-verify", strings.EqualFold(strings.TrimSpace(os.Getenv("VMFLOW_TLS_INSECURE")), "1") || os.Getenv("VMFLOW_TLS_INSECURE") == "true", "skip server certificate verification (dangerous, debug only)"),
	}
}

// Opts returns the parsed client TLS options.
func (f *ClientTLSFlags) Opts() ClientTLSOptions {
	if f == nil {
		return ClientTLSOptions{}
	}
	return ClientTLSOptions{
		CAFile:             strings.TrimSpace(*f.ca),
		ClientCertFile:     strings.TrimSpace(*f.cert),
		ClientKeyFile:      strings.TrimSpace(*f.key),
		InsecureSkipVerify: *f.insecure,
	}
}

// HeaderFlags is a repeatable -H/--header flag. Each entry is "Name: Value" or
// "Name=Value" (curl-style). It is seeded from the VMFLOW_HEADERS environment
// variable (semicolon-separated) so service tokens etc. can be supplied without
// putting secrets on the command line.
type HeaderFlags []string

func (h *HeaderFlags) String() string { return strings.Join(*h, "; ") }

func (h *HeaderFlags) Set(s string) error {
	if s = strings.TrimSpace(s); s != "" {
		*h = append(*h, s)
	}
	return nil
}

// Any reports whether any header is configured.
func (h HeaderFlags) Any() bool { return len(h) > 0 }

// Apply sets the configured headers on req (later entries win per name).
func (h HeaderFlags) Apply(req *http.Request) {
	for _, raw := range h {
		if name, val, ok := splitHeader(raw); ok {
			req.Header.Set(name, val)
		}
	}
}

// HTTPHeader returns the configured headers as an http.Header.
func (h HeaderFlags) HTTPHeader() http.Header {
	hdr := http.Header{}
	for _, raw := range h {
		if name, val, ok := splitHeader(raw); ok {
			hdr.Set(name, val)
		}
	}
	return hdr
}

// AddHeaderFlags registers -H and --header on fs, seeded from VMFLOW_HEADERS
// (semicolon-separated "Name: Value"). Returns the handle.
func AddHeaderFlags(fs *flag.FlagSet) *HeaderFlags {
	hd := HeaderFlags{}
	for _, p := range strings.Split(os.Getenv("VMFLOW_HEADERS"), ";") {
		if p = strings.TrimSpace(p); p != "" {
			hd = append(hd, p)
		}
	}
	fs.Var(&hd, "H", `extra request header as "Name: Value" or "Name=Value" (repeatable; also VMFLOW_HEADERS, ;-separated)`)
	fs.Var(&hd, "header", `alias of -H`)
	return &hd
}

// splitHeader parses a "Name: Value" or "Name=Value" header, trimming spaces.
func splitHeader(raw string) (name, value string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if i := strings.Index(raw, ":"); i > 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:]), true
	}
	if i := strings.Index(raw, "="); i > 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:]), true
	}
	return "", "", false
}
