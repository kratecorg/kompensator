package repo

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// exampleEnv mirrors environments/spanning/example.env.yml: variables and
// nodeVariables at the environment, stack-placement and project-placement
// scopes. It is the reference fixture for the variable-resolution order.
const exampleEnv = `
name: example
stacks:
  - name: stack1
    variables:
      MAIN: "Stack variable"
      STACK_LONG_NAME: "Stack 1"
    nodeVariables:
      customer01:
        MAIN: "Stack variable - Customer01"
        STACK_LONG_NAME: "Stack 1 - Customer01"
      customer02:
        MAIN: "Stack variable - Customer02"
        STACK_LONG_NAME: "Stack 1 - Customer02"
    projects:
      - name: app
        nodes:
          - customer01
          - customer02
        variables:
          MAIN: "Project variable"
          PROJECT_LONG_NAME: "Application Project"
        nodeVariables:
          customer01:
            MAIN: "Project variable - Customer01"
            PROJECT_LONG_NAME: "Application Project - Customer01"
variables:
  MAIN: "Environment variable"
  ENV_LONG_NAME: "Example Environment"
nodeVariables:
  customer02:
    MAIN: "Environment variable - Customer02"
    ENV_LONG_NAME: "Example Environment - Customer02"
`

// TestResolveVariablesExample checks the full nested resolution against the
// documented example: a narrower scope wins over a broader one (so an inner
// all-nodes value beats an outer per-node value), and within a scope the
// per-node layer wins over the all-nodes one.
func TestResolveVariablesExample(t *testing.T) {
	var env Environment
	if err := yaml.Unmarshal([]byte(exampleEnv), &env); err != nil {
		t.Fatalf("unmarshal example env: %v", err)
	}
	placement := env.Stacks[0]

	cases := []struct {
		node string
		want map[string]string
	}{
		{
			node: "customer01",
			want: map[string]string{
				"MAIN":              "Project variable - Customer01", // project+node
				"PROJECT_LONG_NAME": "Application Project - Customer01",
				"STACK_LONG_NAME":   "Stack 1 - Customer01", // stack+node
				"ENV_LONG_NAME":     "Example Environment",  // env only
			},
		},
		{
			node: "customer02",
			want: map[string]string{
				"MAIN":              "Project variable", // project all-nodes beats env/stack per-node
				"PROJECT_LONG_NAME": "Application Project",
				"STACK_LONG_NAME":   "Stack 1 - Customer02",
				"ENV_LONG_NAME":     "Example Environment - Customer02",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.node, func(t *testing.T) {
			// stackDefaults (stacks/<>/stack.yml) is empty in this fixture.
			got := ResolveVariables(nil, env, placement, "app", tc.node)
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s: %s = %q, want %q", tc.node, k, got[k], want)
				}
			}
		})
	}
}

// TestResolveVariablesStackDefaults verifies that stack.yml defaults are the
// lowest layer: they show through when nothing else sets a key, but any
// declared scope overrides them.
func TestResolveVariablesStackDefaults(t *testing.T) {
	var env Environment
	if err := yaml.Unmarshal([]byte(exampleEnv), &env); err != nil {
		t.Fatalf("unmarshal example env: %v", err)
	}
	stackDefaults := map[string]string{
		"MAIN":   "Stack default",  // overridden by every declared scope
		"GLOBAL": "Global default", // set nowhere else
	}

	got := ResolveVariables(stackDefaults, env, env.Stacks[0], "app", "customer02")
	if got["GLOBAL"] != "Global default" {
		t.Errorf("GLOBAL = %q, want %q (stack default should pass through)", got["GLOBAL"], "Global default")
	}
	if got["MAIN"] != "Project variable" {
		t.Errorf("MAIN = %q, want %q (declared scopes override stack default)", got["MAIN"], "Project variable")
	}
}

// TestResolveVariablesNoProjectPlacement covers a project that has no explicit
// placement entry: it inherits the stack-level scopes and contributes no
// project-scoped variables.
func TestResolveVariablesNoProjectPlacement(t *testing.T) {
	var env Environment
	if err := yaml.Unmarshal([]byte(exampleEnv), &env); err != nil {
		t.Fatalf("unmarshal example env: %v", err)
	}

	got := ResolveVariables(nil, env, env.Stacks[0], "other", "customer01")
	if got["MAIN"] != "Stack variable - Customer01" {
		t.Errorf("MAIN = %q, want %q (falls back to stack+node scope)", got["MAIN"], "Stack variable - Customer01")
	}
	if _, ok := got["PROJECT_LONG_NAME"]; ok {
		t.Errorf("PROJECT_LONG_NAME should be unset for a project without placement, got %q", got["PROJECT_LONG_NAME"])
	}
}
