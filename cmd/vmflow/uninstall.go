package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cloudapp3/vmflow/internal/i18n"
	"github.com/cloudapp3/vmflow/internal/uninstaller"
)

// runUninstall implements `vmflow uninstall`: it probes the system, prints the
// removal plan, asks for confirmation, and removes the native service, the
// binary, and owned config/log/cache artifacts. External TLS files are kept.
// `--dry-run` prints the plan without removing anything.
func runUninstall(args []string) {
	runUninstallWithTranslator(args, defaultTranslator())
}

func runUninstallWithTranslator(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), translator.T("cli.uninstall.usage"))
		fs.PrintDefaults()
	}
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, translator.T("cli.uninstall.flag.dry_run"))
	fs.Parse(args)
	if extra := fs.Args(); len(extra) != 0 {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.error.unexpected_arguments", extra))
		os.Exit(1)
	}

	items, warnings := uninstaller.Plan()
	uninstaller.PrintWithTranslator(os.Stdout, items, warnings, translator)

	if dryRun || len(items) == 0 {
		return
	}

	ok, err := uninstaller.ConfirmWithTranslator(os.Stdout, os.Stdin, translator)
	if err != nil {
		fmt.Fprintln(os.Stderr, translator.Tf("cli.uninstall.error.confirm", err))
		os.Exit(1)
	}
	if !ok {
		fmt.Fprintln(os.Stdout, translator.T("cli.uninstall.aborted"))
		return
	}

	if err := uninstaller.ExecuteWithTranslator(os.Stdout, items, translator); err != nil {
		os.Exit(1)
	}
}
