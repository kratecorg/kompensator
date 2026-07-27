package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepoFile creates repoRoot/<rel> with the given content, making parents.
func writeRepoFile(t *testing.T, repoRoot, rel, content string) {
	t.Helper()
	path := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDesiredIdentitiesHonorsProjectPin verifies the orphan reference set: a
// project pinned to another node is NOT desired on this node (so a leftover
// container there is an orphan), while the stack's managed proxy — unpinned —
// still is.
func TestDesiredIdentitiesHonorsProjectPin(t *testing.T) {
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "environments/prod/env.yml", `
name: prod
stacks:
  - name: web
    projects:
      - name: app
        nodes: node2
`)
	writeRepoFile(t, repoRoot, "stacks/web/stack.yml", `
name: web
proxy: traefik
projects:
  - name: app
    compose: compose/app.yml
    strategy: blue-green
`)

	node1, err := desiredIdentities(repoRoot, "node1", []string{"prod"})
	if err != nil {
		t.Fatalf("desiredIdentities node1: %v", err)
	}
	if node1[identityKey("prod", "web", "app")] {
		t.Errorf("app is pinned to node2, must not be desired on node1")
	}
	if !node1[identityKey("prod", "web", "proxy-internal")] {
		t.Errorf("unpinned managed proxy must be desired on node1")
	}

	node2, err := desiredIdentities(repoRoot, "node2", []string{"prod"})
	if err != nil {
		t.Fatalf("desiredIdentities node2: %v", err)
	}
	if !node2[identityKey("prod", "web", "app")] {
		t.Errorf("app is pinned to node2, must be desired there")
	}
}

// TestServiceStatusStateOrphan verifies an orphan row reports the orphan state
// regardless of the desired/running fields.
func TestServiceStatusStateOrphan(t *testing.T) {
	s := ServiceStatus{Orphan: true, Running: "reg/app:v1"}
	if got := s.State(); got != "orphan" {
		t.Errorf("State() = %q, want orphan", got)
	}
}
