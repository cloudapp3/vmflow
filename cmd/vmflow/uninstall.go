package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cloudapp3/vmflow/internal/uninstaller"
)

// runUninstall implements `vmflow uninstall`: it probes the system, prints the
// removal plan, asks for confirmation, and removes the native service, the
// binary, and owned config/log/cache artifacts. External TLS files are kept.
// `--dry-run` prints the plan without removing anything.
func runUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage:\n  vmflow uninstall [--dry-run]\n\n")
		fmt.Fprintf(fs.Output(), "Removes the native service, the vmflow binary, and purges config, logs,\n")
		fmt.Fprintf(fs.Output(), "vmflow-owned cache artifacts, and the self-update cache. External TLS files are preserved.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "print what would be removed without removing it")
	fs.Parse(args)
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %v\n", extra)
		os.Exit(1)
	}

	items, warnings := uninstaller.Plan()
	uninstaller.Print(os.Stdout, items, warnings)

	if dryRun || len(items) == 0 {
		return
	}

	ok, err := uninstaller.Confirm(os.Stdout, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "confirm: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		fmt.Println("aborted")
		return
	}

	if err := uninstaller.Execute(os.Stdout, items); err != nil {
		os.Exit(1)
	}
}
