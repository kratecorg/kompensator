package gitsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishStatusBranchRolling(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	gitDir := filepath.Join(tmp, "statusgit")
	branch := "kompensator/status/c03"

	for i := 1; i <= 13; i++ {
		files := map[string][]byte{
			"status/spanning.yml": []byte(fmt.Sprintf("desiredCommit: commit%d\nhealthy: true\n", i)),
		}
		if err := PublishStatusBranch(ctx, gitDir, origin, branch, files, 10); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// Remote tip must match the last publish, and history must be bounded to 10.
	work := filepath.Join(tmp, "verify")
	if out, err := exec.Command("git", "clone", "-q", "--branch", branch, origin, work).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	count, err := exec.Command("git", "-C", work, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	if got := strings.TrimSpace(string(count)); got != "10" {
		t.Fatalf("remote commit count = %s, want 10", got)
	}
	content, err := os.ReadFile(filepath.Join(work, "status", "spanning.yml"))
	if err != nil {
		t.Fatalf("read tip file: %v", err)
	}
	if !strings.Contains(string(content), "commit13") {
		t.Fatalf("remote tip content = %q, want commit13", content)
	}

	// A no-op publish (identical content) must not create a new commit.
	files := map[string][]byte{"status/spanning.yml": []byte("desiredCommit: commit13\nhealthy: true\n")}
	if err := PublishStatusBranch(ctx, gitDir, origin, branch, files, 10); err != nil {
		t.Fatalf("noop publish: %v", err)
	}
	if out, err := exec.Command("git", "-C", work, "fetch", "-q", "origin", branch).CombinedOutput(); err != nil {
		t.Fatalf("refetch: %v: %s", err, out)
	}
	c3, _ := exec.Command("git", "-C", work, "rev-list", "--count", "FETCH_HEAD").Output()
	if got := strings.TrimSpace(string(c3)); got != "10" {
		t.Fatalf("after noop, remote count = %s, want 10", got)
	}
}
