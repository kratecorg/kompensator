package repo

import (
	"fmt"
	"os"
	"strconv"
)

// defaultManagedFileMode is the permission a managed file gets when the
// declaration does not pin one. Unlike a secret it holds a value that is
// readable in git anyway, so the default is world-readable: its typical
// consumer is a container process running as a different user.
const defaultManagedFileMode = 0o644

// ManagedFile declares one environment-scoped file whose content is the value
// of a single resolved variable.
//
// It exists for one reason: a variable delivered through a project's compose
// environment can only change by recreating that project's containers. The same
// value delivered as a file can be picked up by a reload, so a consumer that
// can reread its configuration never has to restart. Everything else is
// unchanged — the value is declared in env.yml like any other variable, obeys
// the same precedence, and remains visible in a git diff.
//
// Deliberately NOT a template: the content is one variable's value, verbatim.
// A declaration language here would grow conditionals and loops and end up a
// second, weaker copy of compose. A consumer that needs a rendered file should
// render it itself from the values it is given.
type ManagedFile struct {
	Name string `yaml:"name"`
	// Variable names the single variable whose value becomes the file content.
	Variable string `yaml:"variable"`
	// Stack optionally scopes the lookup to one stack placement of this
	// environment, so a value declared for a stack (rather than env-wide) can be
	// delivered. Empty means the environment's own variables only.
	Stack string `yaml:"stack,omitempty"`
	// Nodes optionally pins the file to a subset of the environment's nodes. An
	// empty list means every node that runs the environment receives it.
	Nodes NodeList `yaml:"nodes,omitempty"`
	// Path is the absolute on-node destination.
	Path string `yaml:"path"`
	// Mode is the file's octal permission string (e.g. "0640"). Empty means the
	// default (0644).
	Mode string `yaml:"mode,omitempty"`
	// Reload is the single action to run after the content changed. It follows
	// the same rules as a file secret's reload and is never run when the content
	// is unchanged, so an in-sync reconcile is inert.
	Reload *SecretReload `yaml:"reload,omitempty"`
}

// Validate checks a managed file declaration is well-formed.
func (f ManagedFile) Validate() error {
	if f.Name == "" {
		return fmt.Errorf("managed file has no name")
	}
	if f.Variable == "" {
		return fmt.Errorf("managed file %q names no variable", f.Name)
	}
	if f.Path == "" {
		return fmt.Errorf("managed file %q has an empty path", f.Name)
	}
	if _, err := f.FileMode(); err != nil {
		return fmt.Errorf("managed file %q: %w", f.Name, err)
	}
	if f.Reload != nil && len(f.Reload.Command) > 0 && f.Reload.Recreate != "" {
		return fmt.Errorf("managed file %q: reload sets both command and recreate; pick one", f.Name)
	}
	return nil
}

// FileMode parses the declared octal mode, or the default when omitted.
func (f ManagedFile) FileMode() (os.FileMode, error) {
	if f.Mode == "" {
		return defaultManagedFileMode, nil
	}
	parsed, err := strconv.ParseUint(f.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q (want an octal string like \"0640\"): %w", f.Mode, err)
	}
	return os.FileMode(parsed), nil
}

// TargetsNode reports whether the file should be written on the node. An
// unpinned file targets every node of the environment; a pinned one only the
// nodes in its list. Callers first ensure the node runs the environment.
func (f ManagedFile) TargetsNode(node string) bool {
	return len(f.Nodes) == 0 || f.Nodes.has(node)
}

// ResolveValue returns the value the file should contain on the given node.
//
// The lookup deliberately covers only the env.yml layers (and, when the file
// names a stack, that placement's layers). A stack default is not consulted: a
// value that a switchover changes has to be visible in the environment that a
// human reads and diffs, not inherited from a stack it does not name.
func (f ManagedFile) ResolveValue(env Environment, node string) (string, error) {
	layers := []map[string]string{env.Variables, env.NodeVars(node)}
	if f.Stack != "" {
		placement, ok := env.placement(f.Stack)
		if !ok {
			return "", fmt.Errorf("names stack %q, which this environment does not place", f.Stack)
		}
		layers = append(layers, placement.Variables, placement.NodeVars(node))
	}
	value, ok := MergeVariables(layers...)[f.Variable]
	if !ok {
		return "", fmt.Errorf("variable %s is not declared for node %s", f.Variable, node)
	}
	if value == "" {
		return "", fmt.Errorf("variable %s is empty for node %s", f.Variable, node)
	}
	return value, nil
}
