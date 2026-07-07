package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudapp3/vmflow/controlapi"
	"github.com/cloudapp3/vmflow/tui"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:19090", "relayd control API address")
	token := flag.String("token", os.Getenv("VMFLOW_CONTROL_TOKEN"), "control api bearer token (or VMFLOW_CONTROL_TOKEN)")
	tlsFlags := controlapi.AddClientTLSFlags(flag.CommandLine)
	headerFlags := controlapi.AddHeaderFlags(flag.CommandLine)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var httpClient *http.Client
	if tlsFlags.Opts().Any() {
		hc, err := controlapi.NewHTTPClient(tlsFlags.Opts(), 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tls: %v\n", err)
			os.Exit(1)
		}
		httpClient = hc
	}

	if err := tui.Run(ctx, os.Stdout, *addr, *token, httpClient, headerFlags.HTTPHeader()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
