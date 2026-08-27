package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kratecorg/kompensator/internal/repo"
	"github.com/kratecorg/kompensator/internal/runtime"
)

// managedFileEnv declares one file fed by a stack-scoped variable, which is the
// shape a switchover uses: the value changes in git and the consumer reloads.
func managedFileEnv(path string, nodes ...string) repo.Environment {
	return repo.Environment{
		Name: "preprod",
		Stacks: []repo.StackPlacement{{
			Name:      "carimco",
			Variables: map[string]string{"PG_PRIMARY_NODE": "customer05"},
		}},
		Files: []repo.ManagedFile{{
			Name:     "pg-primary",
			Variable: "PG_PRIMARY_NODE",
			Stack:    "carimco",
			Nodes:    nodes,
			Path:     path,
		}},
	}
}

func materializeOn(t *testing.T, node string, env repo.Environment, home string) error {
	t.Helper()
	opts := Options{Home: home, Env: "preprod"}
	names := runtime.Names{Node: node, Repo: "cd"}
	return materializeManagedFiles(context.Background(), discardLogger(), names, opts, env)
}

func TestManagedFileIsWrittenWithATrailingNewline(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "primary-node")

	if err := materializeOn(t, "customer05", managedFileEnv(path), home); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "customer05\n" {
		t.Fatalf("got %q, want the value plus a newline", content)
	}
}

// The reload hook must not fire on an unchanged value, or every reconcile would
// bounce the consumer. Proven by corrupting the file behind kompensator's back:
// a second pass that skips leaves the corruption in place.
func TestManagedFileIsNotRewrittenWhenTheValueIsUnchanged(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "primary-node")
	env := managedFileEnv(path)

	if err := materializeOn(t, "customer05", env, home); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if err := os.WriteFile(path, []byte("touched by hand\n"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := materializeOn(t, "customer05", env, home); err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "touched by hand\n" {
		t.Fatalf("got %q, want the second pass to have skipped the write", content)
	}
}

func TestManagedFileIsRewrittenWhenTheValueChanges(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "primary-node")

	if err := materializeOn(t, "customer05", managedFileEnv(path), home); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	switched := managedFileEnv(path)
	switched.Stacks[0].Variables["PG_PRIMARY_NODE"] = "customer06"
	if err := materializeOn(t, "customer05", switched, home); err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "customer06\n" {
		t.Fatalf("got %q, want the new value", content)
	}
}

func TestManagedFilePinnedToOtherNodesIsSkipped(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "primary-node")

	if err := materializeOn(t, "customer02", managedFileEnv(path, "customer05", "customer06"), home); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file was written on a node it is not pinned to (stat err: %v)", err)
	}
}

// A missing declaration must stop the reconcile rather than hand the consumer
// an empty file, which it would read as a valid answer.
func TestManagedFileFailsWhenTheVariableIsUndeclared(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "primary-node")
	env := managedFileEnv(path)
	env.Files[0].Variable = "NOT_DECLARED"

	if err := materializeOn(t, "customer05", env, home); err == nil {
		t.Fatal("expected the reconcile to fail on an undeclared variable")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a file was written despite the failure (stat err: %v)", err)
	}
}
