package runtime
package runtime

import (
	"os"
	"strings"
	"testing"
)

func TestLabelsPairsAlwaysMarkManaged(t *testing.T) {
	pairs := Labels{}.pairs()
	if len(pairs) == 0 || pairs[0][0] != LabelManaged || pairs[0][1] != "true" {
		t.Fatalf("empty Labels must still carry %s=true, got %v", LabelManaged, pairs)
	}
}

func TestLabelsPairsOmitEmptyFields(t *testing.T) {
	pairs := Labels{Repo: "cd", Node: "customer02", Env: "prod", Stack: "carimco", Project: "app"}.pairs()
	got := map[string]string{}
	for _, kv := range pairs {
		got[kv[0]] = kv[1]
	}
	if _, ok := got[LabelColor]; ok {
		t.Fatalf("empty Color must not produce a %s label, got %v", LabelColor, got)
	}
	for key, want := range map[string]string{
		LabelManaged: "true",
		LabelRepo:    "cd",
		LabelNode:    "customer02",
		LabelEnv:     "prod",
		LabelStack:   "carimco",
		LabelProject: "app",
	} {
		if got[key] != want {
			t.Errorf("label %s = %q, want %q", key, got[key], want)
		}
	}
}

func TestWriteLabelOverrideStampsEveryService(t *testing.T) {
	labels := Labels{Repo: "cd", Node: "customer02", Env: "prod", Stack: "carimco", Project: "app", Color: "blue"}
	path, err := writeLabelOverride([]string{"frontend", "backend"}, labels)
	if err != nil {
		t.Fatalf("writeLabelOverride: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	content := string(data)
	for _, service := range []string{"frontend", "backend"} {
		if !strings.Contains(content, `"`+service+`":`) {
			t.Errorf("override missing service %q:\n%s", service, content)
		}
	}
	if strings.Count(content, `"`+LabelManaged+`": "true"`) != 2 {
		t.Errorf("expected %s marker on both services:\n%s", LabelManaged, content)
	}
	if !strings.Contains(content, `"`+LabelColor+`": "blue"`) {
		t.Errorf("override missing color label:\n%s", content)
	}
}
