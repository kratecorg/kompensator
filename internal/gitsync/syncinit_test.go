package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSyncInitEmptyRepo(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	dest := filepath.Join(tmp, "repos", "demo")

	commit, err := SyncInit(ctx, origin, "main", dest)
	if err != nil {
		t.Fatalf("SyncInit: %v", err)
	}
	if commit != "" {
		t.Fatalf("commit = %q, want empty for an unborn branch", commit)
	}
	if err := os.WriteFile(filepath.Join(dest, "nodes.yml"), []byte("nodes: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitPush(ctx, dest, "main", "init", "nodes.yml"); err != nil {
		t.Fatalf("CommitPush: %v", err)
	}

	// The branch must now exist on the remote, so a node can clone it.
	if out, err := exec.Command("git", "clone", "-q", "--branch", "main", origin, filepath.Join(tmp, "node")).CombinedOutput(); err != nil {
		t.Fatalf("clone main: %v: %s", err, out)
	}
}

func TestSyncInitMissingBranchKeepsContent(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "master", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	seed := filepath.Join(tmp, "seed")
	mustGit(t, "", "clone", "-q", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	mustGit(t, seed, "push", "-q", "origin", "master")

	dest := filepath.Join(tmp, "repos", "demo")
	if _, err := SyncInit(ctx, origin, "main", dest); err != nil {
		t.Fatalf("SyncInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("new branch lost the default branch's content: %v", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
