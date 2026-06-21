package repo

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// inventoryHeader is prepended to a kompensator-managed inventory/nodes.yml.
const inventoryHeader = "# Nodes and the environments they participate in (managed by kompensator).\n" +
	"# location: an absolute path (local node) or ssh://[user@]host[:port]/path (remote).\n"

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
	// AgeRecipient is the node's public age key ("age1..."). The controller
	// encrypts environment secrets for every node's recipient; the node
	// decrypts them locally with its private key. Empty for nodes provisioned
	// before secrets support.
	AgeRecipient string `yaml:"ageRecipient,omitempty"`
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

// String renders the location back into its canonical form: an absolute path
// for a local node, or "ssh://[user@]host[:port]/path" for a remote one.
func (l Location) String() string {
	if l.Local {
		return l.Path
	}
	host := l.Host
	if l.User != "" {
		host = l.User + "@" + host
	}
	if l.Port != "" {
		host = host + ":" + l.Port
	}
	return "ssh://" + host + l.Path
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

// RecipientsForEnv returns the age recipients of every node that participates
// in the environment and has a recipient set. Used to encrypt the
// environment's secrets so each participating node can decrypt them.
func (inv Inventory) RecipientsForEnv(env string) []string {
	var recipients []string
	for _, n := range inv.Nodes {
		if n.AgeRecipient == "" {
			continue
		}
		for _, e := range n.Environments {
			if e == env {
				recipients = append(recipients, n.AgeRecipient)
				break
			}
		}
	}
	return recipients
}

// index returns the position of the named node, or -1 if absent.
func (inv *Inventory) index(name string) int {
	for i := range inv.Nodes {
		if inv.Nodes[i].Name == name {
			return i
		}
	}
	return -1
}

// Has reports whether the named node exists in the inventory.
func (inv *Inventory) Has(name string) bool {
	return inv.index(name) >= 0
}

// AddNode appends a node. It errors if a node of that name already exists.
func (inv *Inventory) AddNode(name, location, ageRecipient string, envs []string) error {
	if inv.index(name) >= 0 {
		return fmt.Errorf("node %q already in inventory", name)
	}
	sorted := append([]string(nil), envs...)
	sort.Strings(sorted)
	inv.Nodes = append(inv.Nodes, Node{Name: name, Location: location, Environments: sorted, AgeRecipient: ageRecipient})
	return nil
}

// RemoveNode drops the named node, returning the removed entry.
func (inv *Inventory) RemoveNode(name string) (Node, error) {
	i := inv.index(name)
	if i < 0 {
		return Node{}, fmt.Errorf("node %q not in inventory", name)
	}
	n := inv.Nodes[i]
	inv.Nodes = append(inv.Nodes[:i], inv.Nodes[i+1:]...)
	return n, nil
}

// JoinEnv adds the node to an environment (no-op if already a member).
func (inv *Inventory) JoinEnv(name, env string) error {
	i := inv.index(name)
	if i < 0 {
		return fmt.Errorf("node %q not in inventory", name)
	}
	for _, e := range inv.Nodes[i].Environments {
		if e == env {
			return nil
		}
	}
	inv.Nodes[i].Environments = append(inv.Nodes[i].Environments, env)
	sort.Strings(inv.Nodes[i].Environments)
	return nil
}

// LeaveEnv removes the node from an environment (no-op if not a member).
func (inv *Inventory) LeaveEnv(name, env string) error {
	i := inv.index(name)
	if i < 0 {
		return fmt.Errorf("node %q not in inventory", name)
	}
	out := make([]string, 0, len(inv.Nodes[i].Environments))
	for _, e := range inv.Nodes[i].Environments {
		if e != env {
			out = append(out, e)
		}
	}
	inv.Nodes[i].Environments = out
	return nil
}

// Environment is a deployment target, stored at environments/<env>/env.yml. It
// lists which stacks are deployed in this environment.
type Environment struct {
	Name   string   `yaml:"name"`
	Stacks []string `yaml:"stacks"`
	// Variables are environment-specific values injected into every compose
	// project of this environment. They override a stack's own defaults and let
	// e.g. dev and prod use different settings (replica counts, feature flags).
	// The kompensator built-ins (NODE_NAME, ENV_NAME, <SERVICE>_IMAGE/_TAG)
	// always win and cannot be shadowed here.
	Variables map[string]string `yaml:"variables"`
}

// Stack is the env-independent definition of a set of compose projects, stored
// at stacks/<name>/stack.yml.
type Stack struct {
	Name     string    `yaml:"name"`
	Projects []Project `yaml:"projects"`
	// Variables are the stack's own default values for the variables its compose
	// files reference. An environment may override any of them.
	Variables map[string]string `yaml:"variables"`
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

// MergeVariables returns the effective variables for a stack in an environment:
// the stack's own defaults overlaid with the environment's overrides. Neither
// input is mutated; the result is always non-nil.
func MergeVariables(stack, env map[string]string) map[string]string {
	merged := make(map[string]string, len(stack)+len(env))
	for k, v := range stack {
		merged[k] = v
	}
	for k, v := range env {
		merged[k] = v
	}
	return merged
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

// SaveInventory writes inventory/nodes.yml back to the repo, overwriting it
// with a kompensator-managed, generated form (comments other than the header
// are not preserved).
func SaveInventory(repoRoot string, inv Inventory) error {
	var buf bytes.Buffer
	buf.WriteString(inventoryHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(inv); err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	enc.Close()

	path := filepath.Join(repoRoot, "inventory", "nodes.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create inventory dir: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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

// SecretsFile returns the absolute path to a stack's age-encrypted secrets file
// for an environment: environments/<env>/secrets/<stack>.yml.age.
func SecretsFile(repoRoot, env, stack string) string {
	return filepath.Join(repoRoot, "environments", env, "secrets", stack+".yml.age")
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
