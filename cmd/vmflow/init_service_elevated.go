package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/cloudapp3/vmflow/internal/i18n"
)

const (
	initServiceHelperCommand        = "__init-service-install"
	initServiceHelperMaxPayload     = 1 << 20
	initServiceExitConfigRolledBack = 20
	initServiceExitConfigSaved      = 21
)

func runElevatedInitServiceInstall(payload initServicePayload, locale string, output io.Writer) error {
	payload.Locale = locale
	raw, err := json.Marshal(payload)
	if err != nil {
		return unsavedInitServiceError(fmt.Errorf("encode administrator installation request: %w", err))
	}
	if len(raw) > initServiceHelperMaxPayload {
		return unsavedInitServiceError(fmt.Errorf("administrator installation request exceeds %d bytes", initServiceHelperMaxPayload))
	}
	command, cleanup, err := elevatedInitServiceCommand(payload.BinaryPath, locale, raw)
	if err != nil {
		return unsavedInitServiceError(err)
	}
	defer cleanup()
	command.Stdout = output
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		wrapped := fmt.Errorf("administrator service installation failed: %w", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == initServiceExitConfigSaved {
			return savedInitServiceError(wrapped)
		}
		return unsavedInitServiceError(wrapped)
	}
	return nil
}

func elevatedInitServiceCommand(binaryPath, locale string, payload []byte) (*exec.Cmd, func(), error) {
	if len(payload) > initServiceHelperMaxPayload {
		return nil, func() {}, fmt.Errorf("administrator installation request exceeds %d bytes", initServiceHelperMaxPayload)
	}
	switch runtime.GOOS {
	case "windows":
		powershell, err := exec.LookPath("powershell.exe")
		if err != nil {
			return nil, func() {}, fmt.Errorf("PowerShell is required for administrator installation: %w", err)
		}
		file, err := os.CreateTemp("", "vmflow-service-*.json")
		if err != nil {
			return nil, func() {}, fmt.Errorf("create administrator payload: %w", err)
		}
		payloadPath := file.Name()
		cleanup := func() { _ = os.Remove(payloadPath) }
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("secure administrator payload: %w", err)
		}
		if _, err := file.Write(payload); err != nil {
			_ = file.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("write administrator payload: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("sync administrator payload: %w", err)
		}
		if err := file.Close(); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("close administrator payload: %w", err)
		}
		script := fmt.Sprintf(
			`$ErrorActionPreference = 'Stop'; try { $a = @('--lang', $args[1], '__init-service-install', '--payload-file', ('"' + $args[2] + '"')); $p = Start-Process -FilePath $args[0] -ArgumentList $a -Verb RunAs -Wait -PassThru; exit $p.ExitCode } catch { [Console]::Error.WriteLine($_.Exception.Message); exit %d }`,
			initServiceExitConfigRolledBack,
		)
		return exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", script, binaryPath, locale, payloadPath), cleanup, nil
	case "linux", "darwin":
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			return nil, func() {}, fmt.Errorf("sudo is required for administrator installation: %w", err)
		}
		command := exec.Command(sudo, "--", binaryPath, "--lang", locale, initServiceHelperCommand, "--payload-stdin")
		command.Stdin = bytes.NewReader(payload)
		return command, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("administrator installation is not supported on %s", runtime.GOOS)
	}
}

func runInitServiceHelper(args []string, translator *i18n.Translator) {
	translator = translatorOrEnglish(translator)
	fs := flag.NewFlagSet(initServiceHelperCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromStdin := fs.Bool("payload-stdin", false, "")
	payloadFile := fs.String("payload-file", "", "")
	if err := fs.Parse(args); err != nil || len(fs.Args()) != 0 || (*fromStdin == (strings.TrimSpace(*payloadFile) != "")) {
		fmt.Fprintln(os.Stderr, translator.T("cli.init.service.helper.invalid_request"))
		os.Exit(initServiceExitConfigRolledBack)
	}
	if !currentProcessElevated() {
		fmt.Fprintln(os.Stderr, translator.T("cli.init.service.helper.requires_admin"))
		os.Exit(initServiceExitConfigRolledBack)
	}

	var reader io.Reader = os.Stdin
	var payloadPath string
	if !*fromStdin {
		payloadPath = strings.TrimSpace(*payloadFile)
		file, err := openInitServicePayload(payloadPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(initServiceExitConfigRolledBack)
		}
		defer os.Remove(payloadPath)
		defer file.Close()
		reader = file
	}
	payload, err := decodeInitServicePayload(reader)
	if err == nil {
		err = installInitSystemService(payload, os.Stdout)
	}
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, err)
	if initServiceConfigSaved(err) {
		os.Exit(initServiceExitConfigSaved)
	}
	os.Exit(initServiceExitConfigRolledBack)
}

func openInitServicePayload(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect administrator payload: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > initServiceHelperMaxPayload {
		return nil, fmt.Errorf("administrator payload must be a small regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open administrator payload: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect opened administrator payload: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() > initServiceHelperMaxPayload || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("administrator payload changed while opening")
	}
	return file, nil
}

func decodeInitServicePayload(reader io.Reader) (initServicePayload, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, initServiceHelperMaxPayload+1))
	if err != nil {
		return initServicePayload{}, fmt.Errorf("read administrator payload: %w", err)
	}
	if len(raw) > initServiceHelperMaxPayload {
		return initServicePayload{}, fmt.Errorf("administrator payload exceeds %d bytes", initServiceHelperMaxPayload)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload initServicePayload
	if err := decoder.Decode(&payload); err != nil {
		return initServicePayload{}, fmt.Errorf("decode administrator payload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return initServicePayload{}, fmt.Errorf("decode administrator payload: multiple JSON values")
		}
		return initServicePayload{}, fmt.Errorf("decode administrator payload: %w", err)
	}
	return payload, nil
}
