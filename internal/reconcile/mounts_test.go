package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHashMountsIsEmptyWithoutMounts(t *testing.T) {
	hash, err := hashMounts(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("want empty hash without mounts, got %q", hash)
	}
}

func TestHashMountsChangesWithFileContent(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "config", "mimir", "mimir.yaml")
	writeTestFile(t, file, "target: all\n")
	before, err := hashMounts(root, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, "target: ingester\n")
	after, err := hashMounts(root, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("changed file content must change the hash")
	}
}

func TestHashMountsRecursesIntoDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "haproxy")
	writeTestFile(t, filepath.Join(dir, "haproxy.cfg"), "global\n")
	before, err := hashMounts(root, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "maps", "hosts.map"), "a b\n")
	after, err := hashMounts(root, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a new file in a mounted directory must change the hash")
	}
}

func TestHashMountsIgnoresCheckoutLocation(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(first, "a.cfg"), "x\n")
	writeTestFile(t, filepath.Join(second, "a.cfg"), "x\n")
	h1, err := hashMounts(first, []string{filepath.Join(first, "a.cfg")})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashMounts(second, []string{filepath.Join(second, "a.cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("identical content in different checkouts must hash equally")
	}
}

func TestHashMountsToleratesMissingSource(t *testing.T) {
	root := t.TempDir()
	if _, err := hashMounts(root, []string{filepath.Join(root, "absent.cfg")}); err != nil {
		t.Fatalf("missing mount source must not fail: %v", err)
	}
}

func TestIsWithinKeepsOnlyRepoPaths(t *testing.T) {
	cases := map[string]bool{
		"/home/n/repos/cd/stacks/edge/compose/haproxy": true,
		"/home/n/repos/cd":         true,
		"/etc/localtime":           false,
		"/home/n/repos/cd-other/x": false,
	}
	for path, want := range cases {
		if got := isWithin("/home/n/repos/cd", path); got != want {
			t.Errorf("isWithin(%q) = %v, want %v", path, got, want)
		}
	}
}
