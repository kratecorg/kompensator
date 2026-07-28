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

// TestParseManagedProjectsKeepsEmptyColor guards a regression: a managed proxy
// (or any recreate project) has an empty color, so its `docker ps` line ends in
// a tab. The parser must keep all five fields — TrimSpace would eat the trailing
// tab and drop the row, hiding the project from orphan detection.
func TestParseManagedProjectsKeepsEmptyColor(t *testing.T) {
	// Exactly what `docker ps` prints for the customer03 managed proxy: five
	// tab-separated fields, the last (color) empty, then a trailing newline.
	out := "cd-customer03-spanning-carimco-proxy-internal\tspanning\tcarimco\tproxy-internal\t\n"

	got := parseManagedProjects(out)
	if len(got) != 1 {
		t.Fatalf("expected 1 managed project, got %d: %+v", len(got), got)
	}
	want := ManagedProject{
		Name:    "cd-customer03-spanning-carimco-proxy-internal",
		Env:     "spanning",
		Stack:   "carimco",
		Project: "proxy-internal",
		Color:   "",
	}
	if got[0] != want {
		t.Errorf("parsed %+v, want %+v", got[0], want)
	}
}

// TestParseManagedProjectsDedupesAndKeepsColor covers multi-line output with a
// non-empty color and duplicate rows (one per container replica).
func TestParseManagedProjectsDedupesAndKeepsColor(t *testing.T) {
	out := "" +
		"cd-n1-prod-web-app-blue\tprod\tweb\tapp\tblue\n" +
		"cd-n1-prod-web-app-blue\tprod\tweb\tapp\tblue\n" + // second replica, same project
		"cd-n1-prod-web-proxy-internal\tprod\tweb\tproxy-internal\t\n"

	got := parseManagedProjects(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct projects, got %d: %+v", len(got), got)
	}
	if got[0].Color != "blue" {
		t.Errorf("first project color = %q, want blue", got[0].Color)
	}
	if got[1].Project != "proxy-internal" || got[1].Color != "" {
		t.Errorf("second project = %+v, want proxy-internal with empty color", got[1])
	}
}
