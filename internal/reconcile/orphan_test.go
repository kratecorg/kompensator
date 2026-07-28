package reconcile

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"kompensator/internal/repo"
	"kompensator/internal/runtime"
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
// container there is an orphan), and the stack's managed proxy is desired only
// where at least one project actually runs — not on a node whose every project
// is pinned away.
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
	if node1[identityKey("prod", "web", "proxy-internal")] {
		t.Errorf("no project runs on node1, so its managed proxy must not be desired (a leftover proxy there is an orphan)")
	}

	node2, err := desiredIdentities(repoRoot, "node2", []string{"prod"})
	if err != nil {
		t.Fatalf("desiredIdentities node2: %v", err)
	}
	if !node2[identityKey("prod", "web", "app")] {
		t.Errorf("app is pinned to node2, must be desired there")
	}
	if !node2[identityKey("prod", "web", "proxy-internal")] {
		t.Errorf("a project runs on node2, so its managed proxy must be desired there")
	}
}

// TestStackHasProjectOnGatesProxyDeploy verifies the deploy gate: a stack whose
// every project is pinned away from a node is not placed there at all, so its
// managed proxy must not deploy on that node.
func TestStackHasProjectOnGatesProxyDeploy(t *testing.T) {
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

	env, err := repo.LoadEnvironment(repoRoot, "prod")
	if err != nil {
		t.Fatal(err)
	}
	stack, err := repo.LoadStack(repoRoot, "web")
	if err != nil {
		t.Fatal(err)
	}
	placement := env.Stacks[0]

	if stackHasProjectOn(placement, stack, "node1") {
		t.Errorf("node1 has no placed project; stack (and its proxy) must not deploy there")
	}
	if !stackHasProjectOn(placement, stack, "node2") {
		t.Errorf("node2 runs app; stack must deploy there")
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

// TestPruneOrphansSkipsUndefinedEnv verifies the safety guard: when the env is
// not defined in the repo, prune does nothing — and never queries docker, since
// without a desired set it cannot tell an orphan from a legitimate container.
func TestPruneOrphansSkipsUndefinedEnv(t *testing.T) {
	repoRoot := t.TempDir() // no environments/ at all
	names := runtime.Names{Repo: "cd", Node: "node1"}
	opts := Options{Env: "ghost", Prune: true}

	if err := pruneOrphans(context.Background(), slog.New(slog.NewTextHandler(os.Stderr, nil)), names, opts, repoRoot); err != nil {
		t.Fatalf("prune of an undefined env must be a no-op, got: %v", err)
	}
}
