// Package check verifies that a node's bootstrap landed: that the things
// provisioning is meant to set up are actually in place. It runs the checks
// locally for a node home, and on a controller it re-executes itself on every
// inventory node over ssh so one command audits the whole fleet.
package check

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"kompensator/internal/config"
	"kompensator/internal/cron"
	"kompensator/internal/gitsync"
	"kompensator/internal/repo"
	"kompensator/internal/secrets"
	"kompensator/internal/version"
)

// Result is one bootstrap check outcome.
type Result struct {
	Name   string // short check identifier
	OK     bool
	Fixed  bool   // the check found drift and corrected it (state now matches config)
	Detail string // human-readable status (path found, branch, reason it failed)
}

// Node runs the node-local bootstrap checks for a kompensator home: that the
// node config, agent binary, version, age identity, deployment-repo checkout
// and the reconcile crontab entry are present and consistent. self is this
// binary's version; ref is the controller's version to compare against (zero
// when run standalone, in which case the version row is informational).
func Node(ctx context.Context, home string, self, ref version.Info) []Result {
	var out []Result

	cfg, err := config.Load(home)
	if err != nil {
		return []Result{{Name: "config", OK: false, Detail: err.Error()}}
	}
	if cfg.Role != config.RoleNode {
		return []Result{{Name: "config", OK: false, Detail: "home is not a node (no node.yml)"}}
	}
	out = append(out, Result{Name: "config", OK: true, Detail: "node " + cfg.NodeName})

	// Agent binary.
	bin := filepath.Join(home, "kompensator")
	if info, err := os.Stat(bin); err != nil {
		out = append(out, Result{Name: "binary", OK: false, Detail: bin + ": " + reason(err)})
	} else if info.Mode()&0o111 == 0 {
		out = append(out, Result{Name: "binary", OK: false, Detail: bin + ": not executable"})
	} else {
		out = append(out, Result{Name: "binary", OK: true, Detail: bin})
	}

	// Version relative to the controller.
	out = append(out, checkVersion(self, ref))

	// Age identity.
	key := secrets.KeyPath(home)
	if _, err := os.Stat(key); err != nil {
		out = append(out, Result{Name: "age-key", OK: false, Detail: key + ": " + reason(err)})
	} else {
		out = append(out, Result{Name: "age-key", OK: true, Detail: key})
	}

	// Deployment-repo checkout on the configured branch.
	out = append(out, checkRepo(ctx, home, cfg))

	// Reconcile crontab entry must be present AND match the configured schedule.
	out = append(out, checkCron(ctx, home, cfg))

	return out
}

// checkVersion compares this binary's version against the controller's. A node
// may be newer than the controller (e.g. mid-rollout) and still pass; it only
// fails when it is older, since the controller then carries changes the node
// has not picked up. With no reference (a standalone node check) the row is
// informational.
func checkVersion(self, ref version.Info) Result {
	if ref.IsZero() {
		return Result{Name: "version", OK: true, Detail: self.Token()}
	}
	switch version.Compare(self, ref) {
	case 0:
		return Result{Name: "version", OK: true, Detail: self.Token()}
	case 1:
		return Result{Name: "version", OK: true, Detail: fmt.Sprintf("%s (ahead of controller %s)", self.Token(), ref.Token())}
	default:
		return Result{Name: "version", OK: false, Detail: fmt.Sprintf("%s is older than controller %s; run `check --update`", self.Token(), ref.Token())}
	}
}

// checkCron verifies the installed crontab line matches what the node config
// asks for. The crontab is a node-local, safely rewritable artifact, so when it
// drifts from node.yml (a missing entry, or a schedule the operator changed in
// the config but never re-applied) checkCron reconciles it: it reinstalls the
// entry from the config and reports the correction.
func checkCron(ctx context.Context, home string, cfg *config.Config) Result {
	installed, err := cron.InstalledLocal(ctx, home)
	if err != nil {
		return Result{Name: "cron", OK: false, Detail: err.Error()}
	}
	want := cron.Line(home, filepath.Join(home, "kompensator"), cfg.Schedule)
	if installed == want {
		return Result{Name: "cron", OK: true, Detail: cfg.Schedule}
	}

	// Drift: bring the crontab back in line with the config.
	if err := cron.InstallLocal(ctx, home, filepath.Join(home, "kompensator"), cfg.Schedule); err != nil {
		detail := "crontab missing"
		if installed != "" {
			detail = "crontab differs from config"
		}
		return Result{Name: "cron", OK: false, Detail: fmt.Sprintf("%s; could not correct it: %v", detail, err)}
	}
	if installed == "" {
		return Result{Name: "cron", OK: true, Fixed: true, Detail: fmt.Sprintf("entry was missing, installed (schedule %q)", cfg.Schedule)}
	}
	return Result{Name: "cron", OK: true, Fixed: true, Detail: fmt.Sprintf("schedule corrected to %q", cfg.Schedule)}
}

// checkRepo verifies the node's single followed repo is checked out on its
// configured branch. The checkout is a node-local, reconcilable artifact, so on
// drift it self-heals from the config: a missing checkout is cloned, and a repo
// sitting on the wrong branch is switched back to the configured one.
func checkRepo(ctx context.Context, home string, cfg *config.Config) Result {
	if len(cfg.Repos) == 0 {
		return Result{Name: "repo", OK: false, Detail: "no repo configured"}
	}
	r := cfg.Repos[0]
	dest := filepath.Join(config.ReposDir(home), r.Name)

	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		// Missing checkout: clone it the way reconcile would.
		if _, err := gitsync.Sync(ctx, r.URL, r.Branch, dest); err != nil {
			return Result{Name: "repo", OK: false, Detail: fmt.Sprintf("%s: not a git checkout; could not clone it: %v", dest, err)}
		}
		return Result{Name: "repo", OK: true, Fixed: true, Detail: fmt.Sprintf("checkout was missing, cloned %s @ %s", r.Name, r.Branch)}
	}

	branch, err := git(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{Name: "repo", OK: false, Detail: err.Error()}
	}
	branch = strings.TrimSpace(branch)
	if branch != r.Branch {
		// Wrong branch: bring the checkout back to the configured branch.
		if err := healRepoBranch(ctx, dest, r.Branch); err != nil {
			return Result{Name: "repo", OK: false, Detail: fmt.Sprintf("%s on %q, want %q; could not switch: %v", dest, branch, r.Branch, err)}
		}
		return Result{Name: "repo", OK: true, Fixed: true, Detail: fmt.Sprintf("switched %s from %q to %q", r.Name, branch, r.Branch)}
	}
	return Result{Name: "repo", OK: true, Detail: fmt.Sprintf("%s @ %s", r.Name, branch)}
}

// healRepoBranch checks the deployment repo out on the configured branch,
// fetching it first so a local tracking branch can be created if needed.
func healRepoBranch(ctx context.Context, dest, branch string) error {
	if out, err := git(ctx, dest, "fetch", "--quiet", "origin", branch); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if out, err := git(ctx, dest, "checkout", "--quiet", branch); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// Controller audits every node in the controller's inventory. For a local node
// it runs the checks in-process; for a remote node it re-executes the node's
// agent ("kompensator check") over ssh and streams its output, passing its own
// version so each node can flag itself as outdated. When update is set it first
// pushes its own binary onto any node it is strictly newer than. It returns
// false if any node failed a check or could not be reached.
func Controller(ctx context.Context, home string, w io.Writer, self version.Info, update bool) (bool, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return false, err
	}
	if cfg.Role != config.RoleController {
		return false, fmt.Errorf("home %s is not a controller", home)
	}

	seen := map[string]bool{}
	allOK := true
	any := false
	for _, r := range cfg.Repos {
		dest := filepath.Join(config.ReposDir(home), r.Name)
		inv, err := repo.LoadInventory(dest)
		if err != nil {
			return false, fmt.Errorf("repo %q: load inventory: %w", r.Name, err)
		}
		for _, n := range inv.Nodes {
			if seen[n.Name] {
				continue
			}
			seen[n.Name] = true
			any = true
			fmt.Fprintf(w, "\n=== %s (%s) ===\n", n.Name, n.Location)
			ok, err := checkOneNode(ctx, w, n, self, update)
			if err != nil {
				fmt.Fprintf(w, "  error: %v\n", err)
			}
			if !ok {
				allOK = false
			}
		}
	}
	if !any {
		fmt.Fprintln(w, "no nodes in inventory")
	}
	return allOK, nil
}

// checkOneNode checks a single inventory node, locally or over ssh. When update
// is set it first brings the node's binary up to the controller's.
func checkOneNode(ctx context.Context, w io.Writer, n repo.Node, self version.Info, update bool) (bool, error) {
	loc, err := repo.ParseLocation(n.Location)
	if err != nil {
		return false, fmt.Errorf("parse location %q: %w", n.Location, err)
	}

	if update {
		if err := maybeUpdate(ctx, w, loc, self); err != nil {
			fmt.Fprintf(w, "  update skipped: %v\n", err)
		}
	}

	if loc.Local {
		results := Node(ctx, loc.Path, self, self)
		Render(w, results)
		return AllOK(results), nil
	}
	// Remote: re-exec the node's own agent so it runs its local checks, telling
	// it the controller's version so it can flag itself as outdated.
	agent := loc.Path + "/kompensator"
	remote := fmt.Sprintf("%s -home %s check --controller-version %s", agent, loc.Path, self.Token())
	args := append(sshArgs(loc), sshTarget(loc), remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil // a check failed on the node; its output already printed
		}
		return false, err
	}
	return true, nil
}

// maybeUpdate pushes the controller's binary onto the node when the controller
// is strictly newer. A node that is newer, equal, or running a release while
// the controller is a dev build is left untouched (a dev never replaces a
// released binary — Compare encodes that ordering).
func maybeUpdate(ctx context.Context, w io.Writer, loc repo.Location, self version.Info) error {
	nv, err := nodeVersion(ctx, loc)
	if err != nil {
		return fmt.Errorf("query version: %w", err)
	}
	if version.Compare(self, nv) <= 0 {
		return nil // node already at or ahead of the controller
	}
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	if err := pushBinary(ctx, loc, src); err != nil {
		return err
	}
	fmt.Fprintf(w, "  updated binary: %s -> %s\n", nv.Token(), self.Token())
	return nil
}

// nodeVersion asks a node which kompensator version it runs.
func nodeVersion(ctx context.Context, loc repo.Location) (version.Info, error) {
	agent := loc.Path + "/kompensator"
	var out []byte
	var err error
	if loc.Local {
		out, err = exec.CommandContext(ctx, agent, "version").Output()
	} else {
		args := append(sshArgs(loc), sshTarget(loc), agent+" version")
		out, err = exec.CommandContext(ctx, "ssh", args...).Output()
	}
	if err != nil {
		return version.Info{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return version.Info{}, fmt.Errorf("empty version output")
	}
	return version.Parse(fields[len(fields)-1]), nil
}

// pushBinary copies src onto the node's agent path. It writes to a temp name
// and renames into place so a running agent (mid reconcile) is never
// overwritten in flight (rename swaps the directory entry, avoiding ETXTBSY).
func pushBinary(ctx context.Context, loc repo.Location, src string) error {
	dest := loc.Path + "/kompensator"
	tmp := dest + ".new"
	if loc.Local {
		if err := copyFile(src, tmp, 0o755); err != nil {
			return err
		}
		return os.Rename(tmp, dest)
	}
	scp := append(scpArgs(loc), src, sshTarget(loc)+":"+tmp)
	if out, err := exec.CommandContext(ctx, "scp", scp...).CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %w: %s", err, strings.TrimSpace(string(out)))
	}
	args := append(sshArgs(loc), sshTarget(loc), fmt.Sprintf("mv %s %s", tmp, dest))
	if out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh mv: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Render writes results as an aligned table.
func Render(w io.Writer, results []Result) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tOK\tDETAIL")
	for _, r := range results {
		mark := "ok"
		switch {
		case !r.OK:
			mark = "FAIL"
		case r.Fixed:
			mark = "FIXED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, mark, r.Detail)
	}
	tw.Flush()
}

// AllOK reports whether every result passed.
func AllOK(results []Result) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return len(results) > 0
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func sshTarget(loc repo.Location) string {
	if loc.User != "" {
		return loc.User + "@" + loc.Host
	}
	return loc.Host
}

func sshArgs(loc repo.Location) []string {
	args := []string{"-o", "BatchMode=yes"}
	if loc.Port != "" {
		args = append(args, "-p", loc.Port)
	}
	return args
}

func scpArgs(loc repo.Location) []string {
	args := []string{"-o", "BatchMode=yes"}
	if loc.Port != "" {
		args = append(args, "-P", loc.Port)
	}
	return args
}

func reason(err error) string {
	if os.IsNotExist(err) {
		return "missing"
	}
	return err.Error()
}
