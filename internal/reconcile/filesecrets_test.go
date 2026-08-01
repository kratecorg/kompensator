package reconcile

import (
	"slices"
	"testing"
)

func TestParseContainerToken(t *testing.T) {
	cases := map[string]struct {
		arg     string
		wantRef string
		wantOk  bool
	}{
		"bare token":     {arg: "{{container:edge/haproxy}}", wantRef: "edge/haproxy", wantOk: true},
		"no token":       {arg: "kill", wantRef: "", wantOk: false},
		"unterminated":   {arg: "{{container:edge/haproxy", wantRef: "", wantOk: false},
		"embedded token": {arg: "prefix-{{container:s/p}}-suffix", wantRef: "s/p", wantOk: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ref, ok := parseContainerToken(tc.arg)
			if ok != tc.wantOk || ref != tc.wantRef {
				t.Errorf("parseContainerToken(%q) = (%q, %v), want (%q, %v)", tc.arg, ref, ok, tc.wantRef, tc.wantOk)
			}
		})
	}
}

func TestContainerRef(t *testing.T) {
	cmd := []string{"docker", "exec", "{{container:edge/haproxy}}", "kill", "-HUP", "1"}
	ref, ok := containerRef(cmd)
	if !ok || ref != "edge/haproxy" {
		t.Errorf("containerRef = (%q, %v), want (edge/haproxy, true)", ref, ok)
	}
	if _, ok := containerRef([]string{"nginx", "-s", "reload"}); ok {
		t.Error("containerRef should report no token")
	}
}

func TestExpandContainerToken(t *testing.T) {
	cmd := []string{"docker", "exec", "{{container:edge/haproxy}}", "kill", "-HUP", "1"}
	got := expandContainerToken(cmd, "infra-edge-haproxy-1")
	want := []string{"docker", "exec", "infra-edge-haproxy-1", "kill", "-HUP", "1"}
	if !slices.Equal(got, want) {
		t.Errorf("expandContainerToken = %v, want %v", got, want)
	}
}

func TestSplitStackProject(t *testing.T) {
	stack, project, err := splitStackProject("edge/haproxy")
	if err != nil || stack != "edge" || project != "haproxy" {
		t.Errorf("splitStackProject = (%q, %q, %v), want (edge, haproxy, nil)", stack, project, err)
	}
	if _, _, err := splitStackProject("noseparator"); err == nil {
		t.Error("expected error for missing separator")
	}
	if _, _, err := splitStackProject("edge/"); err == nil {
		t.Error("expected error for empty project")
	}
}
