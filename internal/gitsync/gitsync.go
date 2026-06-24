package gitsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Sync ensures the deployment repo at url/branch is checked out at dest and up
// to date. It clones on first use, otherwise fetches and rebases onto the
// remote branch. The node uses a read-only deploy key (configured out of band);
// kompensator never pushes.
//
// It returns the resolved HEAD commit after syncing.
func Sync(ctx context.Context, url, branch, dest string) (commit string, err error) {
	if branch == "" {
		branch = "main"
	}

	if !isGitRepo(dest) {
		if err := clone(ctx, url, branch, dest); err != nil {
			return "", err
		}
	} else if err := pull(ctx, branch, dest); err != nil {
		return "", err
	}

	return head(ctx, dest)
}

func isGitRepo(dest string) bool {
	info, err := os.Stat(filepath.Join(dest, ".git"))
	return err == nil && info.IsDir()
}

func clone(ctx context.Context, url, branch, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}
	out, err := run(ctx, "", "git", "clone", "--branch", branch, "--single-branch", url, dest)
	if err != nil {
		return fmt.Errorf("git clone %s: %w: %s", url, err, out)
	}
	return nil
}

func pull(ctx context.Context, branch, dest string) error {
	if out, err := run(ctx, dest, "git", "fetch", "--quiet", "origin", branch); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, out)
	}
	if out, err := run(ctx, dest, "git", "pull", "--rebase", "--quiet", "origin", branch); err != nil {
		return fmt.Errorf("git pull --rebase: %w: %s", err, out)
	}
	return nil
}

func head(ctx context.Context, dest string) (string, error) {
	out, err := run(ctx, dest, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// CommitPush stages the given paths, commits them with a kompensator identity,
// and pushes to origin/branch. It is a no-op (returns nil) when nothing is
// staged. Unlike the read-only node agent, controller administration commands
// use this to write inventory changes back to the deployment repo.
func CommitPush(ctx context.Context, dir, branch, message string, paths ...string) error {
	if branch == "" {
		branch = "main"
	}
	if out, err := run(ctx, dir, "git", append([]string{"add"}, paths...)...); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	if out, err := run(ctx, dir, "git", "status", "--porcelain"); err != nil {
		return fmt.Errorf("git status: %w: %s", err, out)
	} else if strings.TrimSpace(out) == "" {
		return nil // nothing changed
	}
	if out, err := run(ctx, dir, "git",
		"-c", "user.name=kompensator",
		"-c", "user.email=kompensator@localhost",
		"commit", "-m", message,
	); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	if out, err := run(ctx, dir, "git", "push", "origin", branch); err != nil {
		return fmt.Errorf("git push: %w: %s", err, out)
	}
	return nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitIdentity supplies a deterministic author/committer for kompensator's own
// commits so they never depend on the host's git config.
var gitIdentity = []string{
	"-c", "user.name=kompensator",
	"-c", "user.email=kompensator@localhost",
}

// PublishStatusBranch publishes files onto a dedicated, node-owned status branch
// as a rolling buffer: it keeps a private git directory (gitDir) whose working
// tree is made to contain exactly files, commits the change, trims history to at
// most keep commits, and force-pushes to origin/branch.
//
// The branch is owned solely by this node, so force-pushing is safe and no fetch
// or merge is needed: gitDir is the authoritative copy. When gitDir is absent
// (first run, or a recreated home) it is re-initialised and the remote branch is
// replaced wholesale — acceptable for a rolling status buffer. Nothing is pushed
// when the working tree already matches the last commit.
func PublishStatusBranch(ctx context.Context, gitDir, url, branch string, files map[string][]byte, keep int) error {
	if branch == "" {
		return fmt.Errorf("status branch required")
	}
	if keep < 1 {
		keep = 1
	}
	if err := ensureStatusRepo(ctx, gitDir, url, branch); err != nil {
		return err
	}
	if err := replaceTree(gitDir, files); err != nil {
		return err
	}
	if out, err := run(ctx, gitDir, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	if out, err := run(ctx, gitDir, "git", "status", "--porcelain"); err != nil {
		return fmt.Errorf("git status: %w: %s", err, out)
	} else if strings.TrimSpace(out) == "" {
		return nil // working tree already matches the last commit
	}
	args := append(append([]string{}, gitIdentity...), "commit", "-m", "status update")
	if out, err := run(ctx, gitDir, "git", args...); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	if err := trimStatusHistory(ctx, gitDir, branch, keep); err != nil {
		// Trimming is opportunistic; a failure must never block publishing.
		// Worst case the branch keeps a few extra commits until the next run.
		_ = err
	}
	if out, err := run(ctx, gitDir, "git", "push", "--force", "origin", branch); err != nil {
		return fmt.Errorf("git push --force: %w: %s", err, out)
	}
	return nil
}

// ensureStatusRepo makes gitDir a git repo positioned on branch with origin set
// to url. It initialises an empty repo with an orphan branch on first use.
func ensureStatusRepo(ctx context.Context, gitDir, url, branch string) error {
	if !isGitRepo(gitDir) {
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			return err
		}
		if out, err := run(ctx, gitDir, "git", "init", "-q"); err != nil {
			return fmt.Errorf("git init: %w: %s", err, out)
		}
		if out, err := run(ctx, gitDir, "git", "remote", "add", "origin", url); err != nil {
			return fmt.Errorf("git remote add: %w: %s", err, out)
		}
		if out, err := run(ctx, gitDir, "git", "checkout", "-q", "--orphan", branch); err != nil {
			return fmt.Errorf("git checkout --orphan: %w: %s", err, out)
		}
		return nil
	}
	if out, err := run(ctx, gitDir, "git", "remote", "set-url", "origin", url); err != nil {
		return fmt.Errorf("git remote set-url: %w: %s", err, out)
	}
	if _, err := run(ctx, gitDir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if out, err := run(ctx, gitDir, "git", "checkout", "-q", branch); err != nil {
			return fmt.Errorf("git checkout %s: %w: %s", branch, err, out)
		}
		return nil
	}
	if out, err := run(ctx, gitDir, "git", "checkout", "-q", "--orphan", branch); err != nil {
		return fmt.Errorf("git checkout --orphan %s: %w: %s", branch, err, out)
	}
	return nil
}

// replaceTree makes gitDir's working tree hold exactly files (relative path ->
// content), deleting any other tracked content so removals propagate.
func replaceTree(gitDir string, files map[string][]byte) error {
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(gitDir, e.Name())); err != nil {
			return err
		}
	}
	for rel, content := range files {
		p := filepath.Join(gitDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// trimStatusHistory rewrites branch to keep at most keep commits, re-rooting the
// retained window onto a fresh parentless commit. Each status commit is a full
// snapshot, so the retained commits replay cleanly. The branch name (not HEAD)
// is the rebase target so its ref is what gets updated and pushed.
func trimStatusHistory(ctx context.Context, gitDir, branch string, keep int) error {
	out, err := run(ctx, gitDir, "git", "rev-list", "--count", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-list: %w: %s", err, out)
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("parse commit count %q: %w", strings.TrimSpace(out), err)
	}
	if count <= keep {
		return nil
	}
	upstream, err := run(ctx, gitDir, "git", "rev-parse", fmt.Sprintf("HEAD~%d", keep-1))
	if err != nil {
		return fmt.Errorf("git rev-parse: %w: %s", err, upstream)
	}
	up := strings.TrimSpace(upstream)
	tree, err := run(ctx, gitDir, "git", "rev-parse", up+"^{tree}")
	if err != nil {
		return fmt.Errorf("git rev-parse tree: %w: %s", err, tree)
	}
	mk := append(append([]string{}, gitIdentity...), "commit-tree", strings.TrimSpace(tree), "-m", "status (rolling base)")
	newroot, err := run(ctx, gitDir, "git", mk...)
	if err != nil {
		return fmt.Errorf("git commit-tree: %w: %s", err, newroot)
	}
	rb := append(append([]string{}, gitIdentity...), "rebase", "--onto", strings.TrimSpace(newroot), up, branch)
	cmd := exec.CommandContext(ctx, "git", rb...)
	cmd.Dir = gitDir
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR=:", "GIT_EDITOR=:")
	if out, err := cmd.CombinedOutput(); err != nil {
		_, _ = run(ctx, gitDir, "git", "rebase", "--abort")
		return fmt.Errorf("git rebase --onto: %w: %s", err, string(out))
	}
	return nil
}
