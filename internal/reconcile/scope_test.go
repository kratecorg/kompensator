package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// scopedFileEnv declares three managed files with the three scopes a narrowing
// has to tell apart: one for the named stack, one for a sibling stack, and one
// belonging to the environment itself.
func scopedFileEnv(dir string) repo.Environment {
	return repo.Environment{
		Name:      "preprod",
		Variables: map[string]string{"LOG_LEVEL": "info"},
		Stacks: []repo.StackPlacement{
			{Name: "carimco", Variables: map[string]string{"PG_PRIMARY_NODE": "customer05"}},
			{Name: "edge", Variables: map[string]string{"EDGE_MODE": "active"}},
		},
		Files: []repo.ManagedFile{
			{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Stack: "carimco", Path: filepath.Join(dir, "pg-primary")},
			{Name: "edge-mode", Variable: "EDGE_MODE", Stack: "edge", Path: filepath.Join(dir, "edge-mode")},
			{Name: "log-level", Variable: "LOG_LEVEL", Path: filepath.Join(dir, "log-level")},
		},
	}
}

// A run narrowed to one stack must not deliver another stack's file: the reload
// hook attached to it would act on containers the run promised to leave alone.
func TestNarrowingToAStackSkipsAnotherStacksManagedFile(t *testing.T) {
	dir := t.TempDir()
	env := scopedFileEnv(dir)
	opts := Options{Home: t.TempDir(), Env: "preprod", Stack: "carimco"}

	names := runtime.Names{Node: "customer05", Repo: "cd"}
	if err := materializeManagedFiles(context.Background(), discardLogger(), names, opts, env); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "pg-primary"), "customer05\n")
	assertFileContent(t, filepath.Join(dir, "log-level"), "info\n")
	if _, err := os.Stat(filepath.Join(dir, "edge-mode")); !os.IsNotExist(err) {
		t.Fatalf("the edge stack's file was delivered by a run narrowed to carimco (stat err: %v)", err)
	}
}

func TestAnUnnarrowedRunDeliversEveryManagedFile(t *testing.T) {
	dir := t.TempDir()
	env := scopedFileEnv(dir)
	opts := Options{Home: t.TempDir(), Env: "preprod"}

	names := runtime.Names{Node: "customer05", Repo: "cd"}
	if err := materializeManagedFiles(context.Background(), discardLogger(), names, opts, env); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "pg-primary"), "customer05\n")
	assertFileContent(t, filepath.Join(dir, "edge-mode"), "active\n")
	assertFileContent(t, filepath.Join(dir, "log-level"), "info\n")
}

// A mistyped project name must fail the run: reporting success would tell an
// operator their surgical step ran when nothing happened.
func TestRequireProjectRejectsAnUndeclaredName(t *testing.T) {
	stack := repo.Stack{Projects: []repo.Project{{Name: "infra"}, {Name: "dbproxy"}}}

	if err := requireProject(stack, "dbproxy"); err != nil {
		t.Fatalf("declared project rejected: %v", err)
	}
	if err := requireProject(stack, ""); err != nil {
		t.Fatalf("empty name must select the whole stack: %v", err)
	}
	if err := requireProject(stack, "dbproxyy"); err == nil {
		t.Fatal("an undeclared project name was accepted")
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s: got %q, want %q", path, got, want)
	}
}
