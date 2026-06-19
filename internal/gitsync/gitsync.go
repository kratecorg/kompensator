package gitsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
