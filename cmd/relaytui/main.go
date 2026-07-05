package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudapp3/vmflow/tui"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:19090", "relayd admin API address")
	token := flag.String("token", os.Getenv("VMFLOW_ADMIN_TOKEN"), "admin api bearer token (or VMFLOW_ADMIN_TOKEN)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := tui.Run(ctx, os.Stdout, *addr, *token); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
