package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStackFile(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	path := StackFile(root, "edge")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadStackParsesProjectReload(t *testing.T) {
	root := writeStackFile(t, `name: edge
projects:
  - name: haproxy
    compose: compose/haproxy.yml
    strategy: recreate
    reload:
      signal: SIGUSR2
      service: haproxy
`)
	stack, err := LoadStack(root, "edge")
	if err != nil {
		t.Fatal(err)
	}
	reload := stack.Projects[0].Reload
	if reload == nil || reload.Signal != "SIGUSR2" || reload.Service != "haproxy" {
		t.Fatalf("unexpected reload: %+v", reload)
	}
}

func TestLoadStackRejectsReloadWithoutSignal(t *testing.T) {
	root := writeStackFile(t, `name: edge
projects:
  - name: haproxy
    compose: compose/haproxy.yml
    reload:
      service: haproxy
`)
	_, err := LoadStack(root, "edge")
	if err == nil || !strings.Contains(err.Error(), "reload needs a signal") {
		t.Fatalf("want missing-signal error, got %v", err)
	}
}
