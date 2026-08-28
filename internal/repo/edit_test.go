package repo

import (
	"os"
	"path/filepath"
	"testing"
)

type testProject struct {
	Name     string `yaml:"name"`
	Compose  string `yaml:"compose"`
	Strategy string `yaml:"strategy"`
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stack.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTemp(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAppendToSequenceKeepsCommentsAndBlankLines(t *testing.T) {
	path := writeTemp(t, `# The stack this file describes.
name: shop

# Networks the stack creates.
networks:
  - ${STACK_PREFIX}-internal

projects:
  # The public API.
  - name: api
    compose: compose/api.yml
    strategy: blue-green

# Stack defaults.
variables: {}
`)
	if err := AppendToSequence(path, "projects", testProject{"db", "compose/db.yml", "recreate"}); err != nil {
		t.Fatal(err)
	}
	want := `# The stack this file describes.
name: shop

# Networks the stack creates.
networks:
  - ${STACK_PREFIX}-internal

projects:
  # The public API.
  - name: api
    compose: compose/api.yml
    strategy: blue-green
  - name: db
    compose: compose/db.yml
    strategy: recreate

# Stack defaults.
variables: {}
`
	if got := readTemp(t, path); got != want {
		t.Fatalf("unexpected result:\n%s\nwant:\n%s", got, want)
	}
}

func TestAppendToSequenceConvertsNullKey(t *testing.T) {
	path := writeTemp(t, `name: shop

# Nothing deployed yet.
projects:
`)
	if err := AppendToSequence(path, "projects", testProject{"api", "compose/api.yml", "recreate"}); err != nil {
		t.Fatal(err)
	}
	want := `name: shop

# Nothing deployed yet.
projects:
  - name: api
    compose: compose/api.yml
    strategy: recreate
`
	if got := readTemp(t, path); got != want {
		t.Fatalf("unexpected result:\n%s\nwant:\n%s", got, want)
	}
}

func TestAppendToSequenceAddsMissingKey(t *testing.T) {
	path := writeTemp(t, "name: shop\n")
	if err := AppendToSequence(path, "projects", testProject{"api", "compose/api.yml", "recreate"}); err != nil {
		t.Fatal(err)
	}
	want := `name: shop
projects:
  - name: api
    compose: compose/api.yml
    strategy: recreate
`
	if got := readTemp(t, path); got != want {
		t.Fatalf("unexpected result:\n%s\nwant:\n%s", got, want)
	}
}

func TestAppendToSequenceRejectsNonList(t *testing.T) {
	path := writeTemp(t, "name: shop\nprojects: nope\n")
	if err := AppendToSequence(path, "projects", testProject{}); err == nil {
		t.Fatal("expected an error for a scalar value")
	}
}
