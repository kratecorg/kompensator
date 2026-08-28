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

func TestRemoveFromSequence(t *testing.T) {
	path := writeTemp(t, `name: prod

# Deployed here.
stacks:
  - shop
  - name: myref
    nodes:
      - node7

variables: {}
`)
	removed, err := RemoveFromSequence(path, "stacks", "shop")
	if err != nil || !removed {
		t.Fatalf("remove shop: removed=%t err=%v", removed, err)
	}
	removed, err = RemoveFromSequence(path, "stacks", "myref")
	if err != nil || !removed {
		t.Fatalf("remove myref: removed=%t err=%v", removed, err)
	}
	want := `name: prod

# Deployed here.
stacks: []

variables: {}
`
	if got := readTemp(t, path); got != want {
		t.Fatalf("unexpected result:\n%s\nwant:\n%s", got, want)
	}
	if removed, err := RemoveFromSequence(path, "stacks", "gone"); err != nil || removed {
		t.Fatalf("removing an absent entry: removed=%t err=%v", removed, err)
	}
}

func TestSetStateImage(t *testing.T) {
	path := writeTemp(t, `# Desired images.
app:
  frontend:
    image: registry.example.org/frontend
    tag: v1
  # Publishes the APKs.
  apk:
    image: registry.example.org/apk
    tag: v1
    oneShot: true
`)
	if err := SetStateImage(path, "app", "frontend", "registry.example.org/frontend", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := SetStateImage(path, "app", "apk", "registry.example.org/apk", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := SetStateImage(path, "infra", "postgres", "postgres", "18-alpine"); err != nil {
		t.Fatal(err)
	}
	want := `# Desired images.
app:
  frontend:
    image: registry.example.org/frontend
    tag: v2
  # Publishes the APKs.
  apk:
    image: registry.example.org/apk
    tag: v2
    oneShot: true
infra:
  postgres:
    image: postgres
    tag: 18-alpine
`
	if got := readTemp(t, path); got != want {
		t.Fatalf("unexpected result:\n%s\nwant:\n%s", got, want)
	}
}
