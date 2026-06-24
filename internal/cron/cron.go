// Package cron manages a node's self-reconcile crontab entry. Every node runs
// "kompensator reconcile" on a schedule via the user's crontab; this package
// owns generating, installing, checking and removing that single line.
//
// The entry is tagged with a marker comment unique to the node's home so a host
// can run several agents (one per home) with independent lines, and so install
// and removal are idempotent: install replaces any prior line for the home,
// removal drops it. The same shell snippets drive a local install (via sh) and
// a remote one (the controller runs them over ssh during bootstrap).
package cron

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Marker is the trailing comment that identifies a home's reconcile line. It
// embeds the home path so distinct homes own distinct lines.
func Marker(home string) string {
	return "# kompensator-reconcile:" + home
}

// Line renders the full crontab line for a node home: it runs the node's own
// binary against its home on the given schedule, piping the agent's output to
// syslog (tag "kompensator", facility user) so it lands in the journal instead
// of an unbounded file. binary defaults to <home>/kompensator and schedule to
// "* * * * *" when empty.
func Line(home, binary, schedule string) string {
	if binary == "" {
		binary = home + "/kompensator"
	}
	if schedule == "" {
		schedule = "* * * * *"
	}
	return fmt.Sprintf("%s %s -home %s reconcile 2>&1 | logger -t kompensator -p user.info %s",
		schedule, binary, home, Marker(home))
}

// InstallScript is a POSIX-sh snippet that idempotently installs the line: it
// strips any existing line for this home, then appends the fresh one.
func InstallScript(home, binary, schedule string) string {
	marker := Marker(home)
	line := Line(home, binary, schedule)
	return fmt.Sprintf("{ crontab -l 2>/dev/null | grep -vF %s; printf '%%s\\n' %s; } | crontab -",
		shellQuote(marker), shellQuote(line))
}

// InstalledScript is a POSIX-sh snippet that prints the home's installed
// crontab line (the one carrying its marker), or nothing if absent.
func InstalledScript(home string) string {
	return fmt.Sprintf("crontab -l 2>/dev/null | grep -F %s", shellQuote(Marker(home)))
}

// RemoveScript is a POSIX-sh snippet that drops the home's line, leaving any
// other crontab entries untouched.
func RemoveScript(home string) string {
	marker := Marker(home)
	return fmt.Sprintf("{ crontab -l 2>/dev/null | grep -vF %s; } | crontab -", shellQuote(marker))
}

// InstallLocal installs the reconcile crontab entry on the local host.
func InstallLocal(ctx context.Context, home, binary, schedule string) error {
	return sh(ctx, InstallScript(home, binary, schedule))
}

// RemoveLocal removes the reconcile crontab entry on the local host.
func RemoveLocal(ctx context.Context, home string) error {
	return sh(ctx, RemoveScript(home))
}

// InstalledLocal returns the home's installed crontab line, or "" if no entry
// for this home is present.
func InstalledLocal(ctx context.Context, home string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", InstalledScript(home))
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", nil // grep found nothing
		}
		return "", fmt.Errorf("read crontab: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func sh(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crontab: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote single-quotes a string for safe embedding in a shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
