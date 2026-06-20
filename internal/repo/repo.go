package repo

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Deploy strategies for a project.
const (
	StrategyBlueGreen = "blue-green"
	StrategyRecreate  = "recreate"
)

// Inventory is the set of nodes and the environments they participate in,
// stored at inventory/nodes.yml in the deployment repo.
type Inventory struct {
	Nodes []Node `yaml:"nodes"`
}

// Node is one host in the inventory. A node participates in a set of
// environments and runs every stack (and project) placed in those
// environments; there are no roles.
type Node struct {
	Name string `yaml:"name"`
	// Location tells a controller how to reach the node's kompensator home.
	// It is either an absolute filesystem path (the node is local / directly
	// accessible) or an ssh URL "ssh://[user@]host[:port]/path". The path is
	// the node's KOMPENSATOR_HOME, kept for remote agent operations; the docker
	// status is read from the derived endpoint (local daemon or ssh://).
	Location string `yaml:"location"`
	// Environments this node participates in.
	Environments []string `yaml:"environments"`
}

// Location describes how a controller reaches a node.
type Location struct {
	Local bool   // true when the node shares the controller's host / filesystem
	Path  string // KOMPENSATOR_HOME on the node
	User  string // ssh user (remote only)
	Host  string // ssh host (remote only)
	Port  string // ssh port (remote only, may be empty)
}

// ParseLocation parses a node location string. An absolute path is a local
// node; an "ssh://[user@]host[:port]/path" URL is a remote node.
func ParseLocation(raw string) (Location, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Location{}, fmt.Errorf("empty location")
	}
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Location{}, fmt.Errorf("parse ssh location %q: %w", raw, err)
		}
		if u.Hostname() == "" {
			return Location{}, fmt.Errorf("ssh location %q has no host", raw)
		}
		loc := Location{Host: u.Hostname(), Port: u.Port(), Path: u.Path}
		if u.User != nil {
			loc.User = u.User.Username()
		}
		return loc, nil
	}
	if filepath.IsAbs(raw) {
		return Location{Local: true, Path: raw}, nil
	}
	return Location{}, fmt.Errorf("location %q must be an absolute path or an ssh:// URL", raw)
}

// DockerHost returns the docker "-H" endpoint for the node, or "" for a local
// node (use the local daemon, disambiguated by compose project name).
func (l Location) DockerHost() string {
	if l.Local {
		return ""
	}
	host := l.Host
	if l.User != "" {
		host = l.User + "@" + host
	}
	if l.Port != "" {
		host = host + ":" + l.Port
	}
	return "ssh://" + host
}

// InEnv reports whether the named node participates in the environment.
func (inv Inventory) InEnv(node, env string) bool {
	for _, n := range inv.Nodes {
		if n.Name != node {
			continue
		}
		for _, e := range n.Environments {
			if e == env {
				return true
			}
		}
		return false
	}
	return false
}

// EnvsForNode returns the sorted list of environments the named node
// participates in, or nil if the node is not in the inventory.
func (inv Inventory) EnvsForNode(node string) []string {
	for _, n := range inv.Nodes {
		if n.Name != node {
			continue
		}
		envs := append([]string(nil), n.Environments...)
		sort.Strings(envs)
		return envs
	}
	return nil
}

// Environment is a deployment target, stored at environments/<env>/env.yml. It
// lists which stacks are deployed in this environment.
type Environment struct {
	Name   string   `yaml:"name"`
	Stacks []string `yaml:"stacks"`
}

// Stack is the env-independent definition of a set of compose projects, stored
// at stacks/<name>/stack.yml.
type Stack struct {
	Name     string    `yaml:"name"`
	Projects []Project `yaml:"projects"`
}

// Project is one Docker Compose project within a stack. It switches Blue/Green
// color as a whole (or is recreated in place, for projects that cannot run two
// colors at once, e.g. a database).
type Project struct {
	Name string `yaml:"name"`
	// Compose is the compose file path, relative to the stack directory.
	Compose string `yaml:"compose"`
	// Strategy is "blue-green" (default) or "recreate".
	Strategy string `yaml:"strategy"`
}

// BlueGreen reports whether the project uses the Blue/Green strategy. Any value
// other than "recreate" (including the empty default) means Blue/Green.
func (p Project) BlueGreen() bool {
	return !strings.EqualFold(strings.TrimSpace(p.Strategy), StrategyRecreate)
}

// ServiceImage is the desired image and tag for a single service.
type ServiceImage struct {
	Image string `yaml:"image"`
	Tag   string `yaml:"tag"`
}

// Ref returns the fully qualified image reference (image:tag).
func (s ServiceImage) Ref() string {
	return s.Image + ":" + s.Tag
}

// StackState is the desired state for one stack in one environment, stored at
// environments/<env>/state/<stack>.yml. It maps project name -> service name ->
// desired image/tag.
type StackState map[string]map[string]ServiceImage

// LoadInventory reads inventory/nodes.yml from the checked-out repo root.
func LoadInventory(repoRoot string) (Inventory, error) {
	var inv Inventory
	if err := loadYAML(filepath.Join(repoRoot, "inventory", "nodes.yml"), &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// LoadEnvironment reads environments/<env>/env.yml.
func LoadEnvironment(repoRoot, env string) (Environment, error) {
	var e Environment
	if err := loadYAML(filepath.Join(repoRoot, "environments", env, "env.yml"), &e); err != nil {
		return Environment{}, err
	}
	if e.Name == "" {
		e.Name = env
	}
	return e, nil
}

// StackDir returns the absolute path to a stack's definition directory.
func StackDir(repoRoot, stack string) string {
	return filepath.Join(repoRoot, "stacks", stack)
}

// LoadStack reads stacks/<stack>/stack.yml.
func LoadStack(repoRoot, stack string) (Stack, error) {
	var s Stack
	if err := loadYAML(filepath.Join(StackDir(repoRoot, stack), "stack.yml"), &s); err != nil {
		return Stack{}, err
	}
	if s.Name == "" {
		s.Name = stack
	}
	for i := range s.Projects {
		if s.Projects[i].Strategy == "" {
			s.Projects[i].Strategy = StrategyBlueGreen
		}
	}
	return s, nil
}

// ComposeFile returns the absolute path to a project's compose file.
func ComposeFile(repoRoot, stack string, p Project) string {
	return filepath.Join(StackDir(repoRoot, stack), p.Compose)
}

// LoadStackState reads environments/<env>/state/<stack>.yml as the desired
// state for the stack. A missing file yields an empty state (no desired images
// yet), not an error.
func LoadStackState(repoRoot, env, stack string) (StackState, error) {
	path := filepath.Join(repoRoot, "environments", env, "state", stack+".yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return StackState{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var st StackState
	if err := yaml.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
