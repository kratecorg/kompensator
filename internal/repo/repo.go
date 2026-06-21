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

// RecipientsForNodes returns the age recipients of the named nodes that have a
// recipient set, in inventory order. Used to scope a pinned stack's secrets to
// exactly the nodes that run it.
func (inv Inventory) RecipientsForNodes(names []string) []string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var recipients []string
	for _, n := range inv.Nodes {
		if n.AgeRecipient == "" || !want[n.Name] {
			continue
		}
		recipients = append(recipients, n.AgeRecipient)
	}
	return recipients
}

// NodesForEnv returns the names of every node that participates in the
// environment, in inventory order.
func (inv Inventory) NodesForEnv(env string) []string {
	var names []string
	for _, n := range inv.Nodes {
		for _, e := range n.Environments {
			if e == env {
				names = append(names, n.Name)
				break
			}
		}
	}
	return names
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
	Name   string           `yaml:"name"`
	Stacks []StackPlacement `yaml:"stacks"`
	// Variables are environment-specific values injected into every compose
	// project of this environment. They override a stack's own defaults and let
	// e.g. dev and prod use different settings (replica counts, feature flags).
	// The kompensator built-ins (NODE_NAME, ENV_NAME, <SERVICE>_IMAGE/_TAG)
	// always win and cannot be shadowed here.
	Variables map[string]string `yaml:"variables"`
}

// StackPlacement is one stack listed in an environment, optionally pinned to a
// subset of the environment's nodes at the stack and/or project level. In
// env.yml a stack may be written either as a bare name (it then runs on every
// node that participates in the environment) or as a mapping:
//
//	stacks:
//	  - kratec                # runs on all nodes in the env (the default)
//	  - name: myref
//	    nodes: customer03     # whole stack pinned to one node
//	  - name: carimco         # stack runs everywhere...
//	    projects:
//	      - name: app
//	        nodes: [customer03] # ...but its app project only on customer03
//
// nodes accepts either a single name or a list. Placement only narrows within
// the environment's node pool (the inventory still decides which nodes
// participate at all and how to reach them); it never adds a node that is not a
// member of the environment. A project-level pin narrows further within the
// stack-level pin.
type StackPlacement struct {
	Name string `yaml:"name"`
	// Nodes pins the whole stack to these node names. Empty means "every node in
	// the environment" (no stack-level pin).
	Nodes NodeList `yaml:"nodes,omitempty"`
	// Projects pins individual projects of the stack to a node subset. A project
	// not listed here inherits the stack-level placement.
	Projects []ProjectPlacement `yaml:"projects,omitempty"`
}

// ProjectPlacement pins one project of a stack to a subset of nodes within the
// stack's own placement.
type ProjectPlacement struct {
	Name string `yaml:"name"`
	// Nodes pins the project to these node names. Empty means "wherever the
	// stack runs" (no project-level pin).
	Nodes NodeList `yaml:"nodes,omitempty"`
}

// NodeList is a list of node names that decodes from either a single scalar
// ("customer03") or a YAML sequence (["customer03", "customer04"]).
type NodeList []string

// UnmarshalYAML accepts a scalar or a sequence for a node pin.
func (nl *NodeList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if s != "" {
			*nl = NodeList{s}
		}
		return nil
	}
	var s []string
	if err := value.Decode(&s); err != nil {
		return err
	}
	*nl = NodeList(s)
	return nil
}

// has reports whether the list contains the node (an empty list means "no pin"
// and is handled by the callers, not here).
func (nl NodeList) has(node string) bool {
	for _, n := range nl {
		if n == node {
			return true
		}
	}
	return false
}

// UnmarshalYAML accepts either a bare stack name (scalar) or a mapping with an
// explicit pin, so both forms in env.yml decode into a StackPlacement.
func (s *StackPlacement) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&s.Name)
	}
	type raw StackPlacement
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*s = StackPlacement(r)
	return nil
}

// StackRunsOn reports whether the stack should run on the named node according
// to its stack-level pin. An unpinned stack runs on every node in the
// environment. Project-level pins are checked separately (ProjectRunsOn).
func (s StackPlacement) StackRunsOn(node string) bool {
	return len(s.Nodes) == 0 || s.Nodes.has(node)
}

// ProjectRunsOn reports whether the named project should run on the node. It
// combines the stack-level pin with any project-level pin: the stack must run
// on the node, and if the project carries its own pin the node must also be in
// it.
func (s StackPlacement) ProjectRunsOn(project, node string) bool {
	if !s.StackRunsOn(node) {
		return false
	}
	for _, p := range s.Projects {
		if p.Name == project {
			return len(p.Nodes) == 0 || p.Nodes.has(node)
		}
	}
	return true
}

// StackNames returns the names of every stack placed in the environment, in
// order, regardless of pinning.
func (e Environment) StackNames() []string {
	names := make([]string, len(e.Stacks))
	for i, s := range e.Stacks {
		names[i] = s.Name
	}
	return names
}

// placement returns the StackPlacement for a stack and whether it is listed in
// the environment.
func (e Environment) placement(stack string) (StackPlacement, bool) {
	for _, s := range e.Stacks {
		if s.Name == stack {
			return s, true
		}
	}
	return StackPlacement{}, false
}

// NodesRunningStack returns the subset of envNodes that run at least one of the
// stack's projects, honoring both stack- and project-level pins. It is used to
// scope a stack's secrets to exactly the nodes that run it. When projects is
// empty (project list unknown) it falls back to the stack-level placement.
func (e Environment) NodesRunningStack(stack string, projects, envNodes []string) []string {
	p, ok := e.placement(stack)
	if !ok {
		return nil
	}
	var out []string
	for _, node := range envNodes {
		if !p.StackRunsOn(node) {
			continue
		}
		if len(projects) == 0 {
			out = append(out, node)
			continue
		}
		for _, proj := range projects {
			if p.ProjectRunsOn(proj, node) {
				out = append(out, node)
				break
			}
		}
	}
	return out
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
