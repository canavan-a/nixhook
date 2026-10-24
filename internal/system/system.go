// Package system wraps the host commands nixhook needs: systemd, the system
// profile, and small exec helpers.
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// JobUnit is the fixed name of the transient unit a deploy runs in. systemd
// refuses to start a second unit with the same name, which is what limits a
// box to one deploy at a time.
const JobUnit = "nixhook-job"

const systemProfile = "/nix/var/nix/profiles/system"

var generationRe = regexp.MustCompile(`^system-(\d+)-link$`)

// ErrJobRunning is returned by StartJob when a deploy is already running.
var ErrJobRunning = errors.New("a deploy is already running")

// Output runs a command and returns its trimmed stdout. On failure the error
// includes stderr.
func Output(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Generation returns the number of the current system profile generation.
func Generation() (int, error) {
	link, err := os.Readlink(systemProfile)
	if err != nil {
		return 0, err
	}
	m := generationRe.FindStringSubmatch(filepath.Base(link))
	if m == nil {
		return 0, fmt.Errorf("unexpected system profile link %q", link)
	}
	return strconv.Atoi(m[1])
}

// NixOSVersion returns the version of the running system, or "" if unknown.
func NixOSVersion() string {
	b, err := os.ReadFile("/run/current-system/nixos-version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// State returns the output of systemctl is-system-running, e.g. "running" or
// "degraded". The command exits non-zero for anything but running, so its
// output is used rather than its exit status.
func State(ctx context.Context) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out))
}

// FailedUnits lists the units systemd currently reports as failed.
func FailedUnits(ctx context.Context) ([]string, error) {
	out, err := Output(ctx, "systemctl", "list-units", "--state=failed", "--plain", "--no-legend", "--full")
	if err != nil {
		return nil, err
	}
	units := []string{}
	for line := range strings.Lines(out) {
		if fields := strings.Fields(line); len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

// JobActive reports whether the deploy unit is running.
func JobActive(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", JobUnit).Run() == nil
}

// StartJob runs exe with args in the transient deploy unit so it outlives the
// server process. env entries are KEY=VALUE pairs passed into the unit.
func StartJob(ctx context.Context, description string, env []string, exe string, args ...string) error {
	if JobActive(ctx) {
		return ErrJobRunning
	}
	runArgs := []string{
		"--unit=" + JobUnit,
		"--collect",
		"--quiet",
		"--property=Type=exec",
		"--description=" + description,
	}
	for _, kv := range env {
		runArgs = append(runArgs, "--setenv="+kv)
	}
	runArgs = append(runArgs, "--", exe)
	runArgs = append(runArgs, args...)
	if _, err := Output(ctx, "systemd-run", runArgs...); err != nil {
		if JobActive(ctx) {
			return ErrJobRunning
		}
		return err
	}
	return nil
}
