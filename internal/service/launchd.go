package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type launchdCommandRunner func([]string) ([]byte, error)

type launchdPlistSnapshot struct {
	exists     bool
	mode       os.FileMode
	data       []byte
	linkTarget string
}

func installLaunchdDaemon(cfg Config, path string, runner launchdCommandRunner) error {
	previous, err := snapshotLaunchdPlist(path)
	if err != nil {
		return fmt.Errorf("snapshot existing plist: %w", err)
	}

	label := launchdLabel(cfg)
	state, err := probeLaunchdService(label, runner)
	if err != nil {
		return fmt.Errorf("inspect existing launchd daemon %s: %w", label, err)
	}
	wasLoaded := state.loaded
	if wasLoaded && !sameLaunchdPlistPath(state.sourcePath, path) {
		return fmt.Errorf("launchd daemon %s is loaded from %q, not %s; refusing an update that cannot be rolled back safely", label, state.sourcePath, path)
	}
	if wasLoaded && !previous.exists {
		return fmt.Errorf("launchd daemon %s is loaded but %s does not exist; refusing an update that cannot be rolled back", label, path)
	}

	if err := writeFileAtomic(path, []byte(launchdPlist(cfg)), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	if wasLoaded {
		if err := runLaunchdCommand(runner, []string{"launchctl", "bootout", "system/" + label}); err != nil {
			return finishFailedLaunchdInstall(path, label, previous, wasLoaded, false, runner, err)
		}
	}
	if err := runLaunchdCommand(runner, []string{"launchctl", "bootstrap", "system", path}); err != nil {
		return finishFailedLaunchdInstall(path, label, previous, wasLoaded, true, runner, err)
	}
	return nil
}

func finishFailedLaunchdInstall(path, label string, previous launchdPlistSnapshot, wasLoaded, newBootstrapAttempted bool, runner launchdCommandRunner, installErr error) error {
	rollbackErr := rollbackLaunchdInstall(path, label, previous, wasLoaded, newBootstrapAttempted, runner)
	if rollbackErr != nil {
		return fmt.Errorf("%w; rollback incomplete: %v", installErr, rollbackErr)
	}
	return fmt.Errorf("%w; previous plist and launchd state restored", installErr)
}

func rollbackLaunchdInstall(path, label string, previous launchdPlistSnapshot, wasLoaded, newBootstrapAttempted bool, runner launchdCommandRunner) error {
	var problems []string
	stateKnown := true
	stillLoaded := false
	if newBootstrapAttempted {
		// A failed bootstrap can still leave a partially loaded new job. Clean it
		// up, but only probe when bootout itself cannot confirm the result.
		if _, err := runner([]string{"launchctl", "bootout", "system/" + label}); err != nil {
			state, probeErr := probeLaunchdService(label, runner)
			stillLoaded = state.loaded
			if probeErr != nil {
				stateKnown = false
				problems = append(problems, fmt.Sprintf("determine launchd state after failed cleanup: %v", probeErr))
			} else if stillLoaded && !sameLaunchdPlistPath(state.sourcePath, path) {
				stateKnown = false
				problems = append(problems, fmt.Sprintf("unexpected launchd source after failed cleanup: %q", state.sourcePath))
			}
		}
	} else {
		// The initial bootout failed before a new job was attempted. Do not send a
		// second destructive bootout: the old daemon may still be healthy.
		state, probeErr := probeLaunchdService(label, runner)
		stillLoaded = state.loaded
		if probeErr != nil {
			stateKnown = false
			problems = append(problems, fmt.Sprintf("determine launchd state after failed bootout: %v", probeErr))
		} else if stillLoaded && !sameLaunchdPlistPath(state.sourcePath, path) {
			stateKnown = false
			problems = append(problems, fmt.Sprintf("unexpected launchd source after failed bootout: %q", state.sourcePath))
		}
	}

	if err := restoreLaunchdPlist(path, previous); err != nil {
		problems = append(problems, fmt.Sprintf("restore plist: %v", err))
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	if !stateKnown {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}

	if wasLoaded {
		switch {
		case !stillLoaded:
			if err := runLaunchdCommand(runner, []string{"launchctl", "bootstrap", "system", path}); err != nil {
				problems = append(problems, fmt.Sprintf("restore previous daemon: %v", err))
			}
		case newBootstrapAttempted:
			problems = append(problems, "new launchd daemon still owns the label after rollback bootout")
		}
	} else if stillLoaded {
		problems = append(problems, "new launchd daemon still owns the label after first-install rollback")
	}

	if len(problems) != 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func snapshotLaunchdPlist(path string) (launchdPlistSnapshot, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return launchdPlistSnapshot{}, nil
	}
	if err != nil {
		return launchdPlistSnapshot{}, err
	}

	snapshot := launchdPlistSnapshot{exists: true, mode: info.Mode()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return launchdPlistSnapshot{}, err
		}
		snapshot.linkTarget = target
		return snapshot, nil
	}
	if !info.Mode().IsRegular() {
		return launchdPlistSnapshot{}, fmt.Errorf("existing plist path %s is not a regular file or symlink", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return launchdPlistSnapshot{}, err
	}
	snapshot.data = data
	return snapshot, nil
}

func restoreLaunchdPlist(path string, snapshot launchdPlistSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if snapshot.mode&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(snapshot.linkTarget, path)
	}
	return writeFileAtomic(path, snapshot.data, snapshot.mode.Perm())
}

type launchdServiceState struct {
	loaded     bool
	sourcePath string
}

func probeLaunchdService(label string, runner launchdCommandRunner) (launchdServiceState, error) {
	argv := []string{"launchctl", "print", "system/" + label}
	out, err := runner(argv)
	if err == nil {
		return launchdServiceState{loaded: true, sourcePath: launchdPlistSourcePath(out)}, nil
	}
	if launchdServiceNotFound(out, err) {
		return launchdServiceState{}, nil
	}
	return launchdServiceState{}, fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
}

func launchdPlistSourcePath(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "path = ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "path = ")), `"`)
		}
	}
	return ""
}

func sameLaunchdPlistPath(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && filepath.Clean(left) == filepath.Clean(right)
}

func launchdServiceNotFound(out []byte, err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 {
		return true
	}
	message := strings.ToLower(string(out))
	return strings.Contains(message, "could not find service") || strings.Contains(message, "service not found")
}

func runLaunchdCommand(runner launchdCommandRunner, argv []string) error {
	out, err := runner(argv)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w (%s)", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
}
