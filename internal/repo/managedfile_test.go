package repo

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The declaration syntax is part of the contract with the deployment repo, so it
// is asserted against literal YAML rather than a hand-built struct.
func TestManagedFileDecodesFromEnvironmentYAML(t *testing.T) {
	const document = `
name: preprod
files:
  - name: pg-primary
    variable: PG_PRIMARY_NODE
    stack: carimco
    nodes:
      - customer05
      - customer06
    path: /home/peter/carimco/db/primary-node
    reload:
      command: [docker, exec, "{{container:carimco/dbproxy}}", /usr/local/bin/pgbouncer-set-primary.sh]
`
	var env Environment
	if err := yaml.Unmarshal([]byte(document), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(env.Files))
	}

	file := env.Files[0]
	if err := file.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if file.Variable != "PG_PRIMARY_NODE" || file.Stack != "carimco" {
		t.Fatalf("got variable %q, stack %q", file.Variable, file.Stack)
	}
	if file.Path != "/home/peter/carimco/db/primary-node" {
		t.Fatalf("got path %q", file.Path)
	}
	if !file.TargetsNode("customer06") || file.TargetsNode("customer02") {
		t.Fatal("node pin was not decoded")
	}
	if file.Reload == nil || len(file.Reload.Command) != 4 {
		t.Fatalf("reload command was not decoded: %+v", file.Reload)
	}
}

// envWithStackVariable mirrors how a switchover topology is declared: the value
// lives on the stack placement, not env-wide, because two projects of that one
// stack consume it.
func envWithStackVariable() Environment {
	return Environment{
		Name: "preprod",
		Variables: map[string]string{
			"PG_PRIMARY_NODE": "env-wide",
			"UNRELATED":       "x",
		},
		NodeVariables: map[string]map[string]string{
			"customer06": {"PG_PRIMARY_NODE": "env-wide-node"},
		},
		Stacks: []StackPlacement{{
			Name:      "carimco",
			Variables: map[string]string{"PG_PRIMARY_NODE": "customer05"},
			NodeVariables: map[string]map[string]string{
				"customer06": {"PG_PRIMARY_NODE": "placement-node"},
			},
		}},
	}
}

func TestManagedFileResolvesFromNamedStack(t *testing.T) {
	file := ManagedFile{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Stack: "carimco", Path: "/tmp/x"}

	value, err := file.ResolveValue(envWithStackVariable(), "customer05")
	if err != nil {
		t.Fatalf("ResolveValue: %v", err)
	}
	if value != "customer05" {
		t.Fatalf("got %q, want the stack placement's value", value)
	}
}

func TestManagedFilePrefersTheNarrowestLayer(t *testing.T) {
	file := ManagedFile{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Stack: "carimco", Path: "/tmp/x"}

	value, err := file.ResolveValue(envWithStackVariable(), "customer06")
	if err != nil {
		t.Fatalf("ResolveValue: %v", err)
	}
	if value != "placement-node" {
		t.Fatalf("got %q, want the placement's per-node override", value)
	}
}

func TestManagedFileWithoutStackSeesOnlyTheEnvironment(t *testing.T) {
	file := ManagedFile{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Path: "/tmp/x"}

	value, err := file.ResolveValue(envWithStackVariable(), "customer05")
	if err != nil {
		t.Fatalf("ResolveValue: %v", err)
	}
	if value != "env-wide" {
		t.Fatalf("got %q, want the env-wide value", value)
	}
}

func TestManagedFileRejectsAStackTheEnvironmentDoesNotPlace(t *testing.T) {
	file := ManagedFile{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Stack: "elsewhere", Path: "/tmp/x"}

	if _, err := file.ResolveValue(envWithStackVariable(), "customer05"); err == nil {
		t.Fatal("expected an error for a stack that is not placed here")
	}
}

// An undeclared variable must not silently produce an empty file: the consumer
// would read it as a valid answer.
func TestManagedFileRejectsAnUndeclaredVariable(t *testing.T) {
	file := ManagedFile{Name: "pg-primary", Variable: "NOT_DECLARED", Stack: "carimco", Path: "/tmp/x"}

	if _, err := file.ResolveValue(envWithStackVariable(), "customer05"); err == nil {
		t.Fatal("expected an error for an undeclared variable")
	}
}

func TestManagedFileRejectsAnEmptyValue(t *testing.T) {
	env := Environment{Variables: map[string]string{"PG_PRIMARY_NODE": ""}}
	file := ManagedFile{Name: "pg-primary", Variable: "PG_PRIMARY_NODE", Path: "/tmp/x"}

	if _, err := file.ResolveValue(env, "customer05"); err == nil {
		t.Fatal("expected an error for an empty value")
	}
}

func TestManagedFileValidateRequiresNameVariableAndPath(t *testing.T) {
	cases := map[string]ManagedFile{
		"no name":     {Variable: "V", Path: "/tmp/x"},
		"no variable": {Name: "f", Path: "/tmp/x"},
		"no path":     {Name: "f", Variable: "V"},
		"bad mode":    {Name: "f", Variable: "V", Path: "/tmp/x", Mode: "rw-r--r--"},
		"both reload actions": {
			Name: "f", Variable: "V", Path: "/tmp/x",
			Reload: &SecretReload{Command: []string{"true"}, Recreate: "s/p"},
		},
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			if err := file.Validate(); err == nil {
				t.Fatal("expected Validate to reject this declaration")
			}
		})
	}
}

func TestManagedFileModeDefaultsToWorldReadable(t *testing.T) {
	mode, err := ManagedFile{Name: "f", Variable: "V", Path: "/tmp/x"}.FileMode()
	if err != nil {
		t.Fatalf("FileMode: %v", err)
	}
	if mode != 0o644 {
		t.Fatalf("got %04o, want 0644", mode)
	}
}
