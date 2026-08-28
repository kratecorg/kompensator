package repo

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// inventoryHeader is prepended to a kompensator-managed inventory/nodes.yml.
const inventoryHeader = "# Nodes known to this deployment repo (managed by kompensator).\n" +
	"# Nodes are just nodes; which node runs which environment is decided by the\n" +
	"# environment definitions (environments/<env>/env.yml), not here.\n" +
	"# location: an absolute path (local node) or ssh://[user@]host[:port]/path (remote).\n"

// Deploy strategies for a project.
const (
	StrategyBlueGreen = "blue-green"
	StrategyRecreate  = "recreate"
)

// Inventory is the set of nodes a deployment repo knows about, stored at
// inventory/nodes.yml. Nodes are just nodes: which node runs which environment
// is decided entirely by the environment definitions, not here.
type Inventory struct {
	Nodes []Node `yaml:"nodes"`
}

// Node is one host in the inventory. Every node is a candidate for every
// environment defined in the repo; an environment (or a stack/project within
// it) may pin itself to a node subset, but the inventory itself carries no
// environment membership.
type Node struct {
	Name string `yaml:"name"`
	// Location tells a controller how to reach the node's kompensator home.
	// It is either an absolute filesystem path (the node is local / directly
	// accessible) or an ssh URL "ssh://[user@]host[:port]/path". The path is
	// the node's KOMPENSATOR_HOME, kept for remote agent operations; the docker
	// status is read from the derived endpoint (local daemon or ssh://).
	Location string `yaml:"location"`
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

// AllNodeNames returns the names of every node in the inventory, in order.
// Every node is a candidate for every environment; placement narrows this.
func (inv Inventory) AllNodeNames() []string {
	names := make([]string, len(inv.Nodes))
	for i, n := range inv.Nodes {
		names[i] = n.Name
	}
	return names
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
func (inv *Inventory) AddNode(name, location, ageRecipient string) error {
	if inv.index(name) >= 0 {
		return fmt.Errorf("node %q already in inventory", name)
	}
	inv.Nodes = append(inv.Nodes, Node{Name: name, Location: location, AgeRecipient: ageRecipient})
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

// Environment is a deployment target, stored at environments/<env>/env.yml. It
// lists which stacks are deployed in this environment.
type Environment struct {
	Name   string           `yaml:"name"`
	Stacks []StackPlacement `yaml:"stacks"`
	// Networks and Volumes are docker resources kompensator creates (idempotently)
	// before deploying any stack of this environment. Use them for resources that
	// span stacks; per-stack resources belong on the stack instead. Names may
	// contain the env-scoped built-ins (${ENV_NAME}, ${NODE_NAME}, ${REPO_NAME}).
	Networks []ManagedResource `yaml:"networks,omitempty"`
	Volumes  []ManagedResource `yaml:"volumes,omitempty"`
	// Variables are environment-specific values injected into every compose
	// project of this environment. They override a stack's own defaults and let
	// e.g. dev and prod use different settings (replica counts, feature flags).
	// The kompensator built-ins (NODE_NAME, ENV_NAME, <SERVICE>_IMAGE/_TAG)
	// always win and cannot be shadowed here.
	Variables map[string]string `yaml:"variables"`
	// NodeVariables are per-node overrides keyed by node name. They are layered
	// on top of Variables for the node currently reconciling, letting a single
	// node diverge (e.g. a node that hosts a service reaches it in-network while
	// the others reach it across the WireGuard mesh). Only the entry for the
	// reconciling node applies; the built-ins still win over everything.
	NodeVariables map[string]map[string]string `yaml:"nodeVariables,omitempty"`
	// Secrets are environment-scoped file secrets: opaque, age-encrypted blobs
	// (e.g. a TLS certificate bundle) that kompensator materialises to a host
	// path on the nodes that need them, out of band from the compose env-var
	// secrets. See EnvSecret for the declaration and materialisation model.
	Secrets []EnvSecret `yaml:"secrets,omitempty"`
	// Files are environment-scoped managed files: a single declared variable
	// delivered to a host path instead of (or alongside) a compose environment
	// variable, so a consumer can reload on change rather than be recreated. See
	// ManagedFile.
	Files []ManagedFile `yaml:"files,omitempty"`
}

// NodeVars returns the per-node variable overrides for the named node, or nil
// if the environment defines none for it.
func (e Environment) NodeVars(node string) map[string]string {
	return e.NodeVariables[node]
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
	// Variables are values injected for this stack in this environment. In the
	// variable-resolution order they sit above the environment-wide variables and
	// below the per-project ones, letting "this stack in this env" diverge without
	// touching the env-wide defaults (see the merge order in reconcileRepo).
	Variables map[string]string `yaml:"variables,omitempty"`
	// NodeVariables are per-node overrides for this stack placement, keyed by node
	// name. Only the reconciling node's entry applies and it wins over Variables.
	NodeVariables map[string]map[string]string `yaml:"nodeVariables,omitempty"`
}

// NodeVars returns this stack placement's per-node variable overrides for the
// named node, or nil if none are defined.
func (s StackPlacement) NodeVars(node string) map[string]string {
	return s.NodeVariables[node]
}

// Project returns the placement entry for the named project, or the zero value
// if the project has no explicit placement (it then inherits the stack-level
// placement and carries no project-scoped variables).
func (s StackPlacement) Project(name string) ProjectPlacement {
	for _, p := range s.Projects {
		if p.Name == name {
			return p
		}
	}
	return ProjectPlacement{}
}

// ProjectPlacement pins one project of a stack to a subset of nodes within the
// stack's own placement.
type ProjectPlacement struct {
	Name string `yaml:"name"`
	// Nodes pins the project to these node names. Empty means "wherever the
	// stack runs" (no project-level pin).
	Nodes NodeList `yaml:"nodes,omitempty"`
	// Variables are values injected for this project in this environment. They are
	// the most specific non-node layer and win over the stack- and env-level
	// variables (see the merge order in reconcileRepo).
	Variables map[string]string `yaml:"variables,omitempty"`
	// NodeVariables are per-node overrides for this project placement, keyed by
	// node name. Only the reconciling node's entry applies and it is the most
	// specific layer of all — it wins over every other variable source.
	NodeVariables map[string]map[string]string `yaml:"nodeVariables,omitempty"`
}

// NodeVars returns this project placement's per-node variable overrides for the
// named node, or nil if none are defined.
func (p ProjectPlacement) NodeVars(node string) map[string]string {
	return p.NodeVariables[node]
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

// RunsOnNode reports whether the named node runs at least one stack of this
// environment, i.e. is a deploy target for it. A node runs the environment
// unless every stack is pinned away from it.
func (e Environment) RunsOnNode(node string) bool {
	for _, p := range e.Stacks {
		if p.StackRunsOn(node) {
			return true
		}
	}
	return false
}

// Stack is the env-independent definition of a set of compose projects, stored
// at stacks/<name>/stack.yml.
type Stack struct {
	Name     string    `yaml:"name"`
	Projects []Project `yaml:"projects"`
	// Networks and Volumes are docker resources kompensator creates (idempotently)
	// before deploying any of the stack's projects, so no project has to "own" a
	// shared network/volume via compose: every project simply joins them as
	// external. This removes the deploy-ordering coupling between an owner project
	// and the projects that reference it. Names may contain the stack-scoped
	// built-ins (e.g. ${STACK_PREFIX}); they are resolved by kompensator.
	Networks []ManagedResource `yaml:"networks,omitempty"`
	Volumes  []ManagedResource `yaml:"volumes,omitempty"`
	// Proxy, when set, makes kompensator run a stack-internal reverse proxy for
	// this stack — there is no hand-written compose file for it. It is the
	// default target of the stack's project proxy bindings (see ManagedProxy).
	Proxy *ManagedProxy `yaml:"proxy,omitempty"`
	// Variables are the stack's own default values for the variables its compose
	// files reference. An environment may override any of them.
	Variables map[string]string `yaml:"variables"`
}

// ManagedResource is a docker network or volume kompensator creates before any
// project of the stack/environment deploys, so projects never own a shared
// resource via compose — they all reference it as external. It decodes from a
// bare name (scalar) or a mapping {name, driver, options}. The name may contain
// the identity built-ins (${STACK_PREFIX}, ${ENV_NAME}, ${NODE_NAME}, ...),
// resolved by kompensator before the resource is created.
type ManagedResource struct {
	Name    string            `yaml:"name"`
	Driver  string            `yaml:"driver,omitempty"`
	Options map[string]string `yaml:"options,omitempty"`
}

// UnmarshalYAML accepts a scalar resource name or a {name, driver, options}
// mapping.
func (r *ManagedResource) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&r.Name)
	}
	type raw ManagedResource
	var rr raw
	if err := value.Decode(&rr); err != nil {
		return err
	}
	*r = ManagedResource(rr)
	return nil
}

// defaultProxyName is the logical name of a stack's managed proxy when the
// stack does not name it explicitly. It is also the default target of the
// stack's project proxy bindings.
const defaultProxyName = "internal"

// ManagedProxy declares a stack-internal reverse proxy that kompensator
// synthesizes and runs for the stack itself: it renders the proxy container,
// joins it to the stack's network(s), and feeds it the dynamic routing config
// produced on each Blue/Green switch (see internal/proxy). The end user never
// writes a compose file for it. A user-managed edge/global proxy placed in
// front of the stack is a separate, advanced concern kompensator does not touch.
//
// In stack.yml it is written either as a bare kind (the common case) or as a
// mapping for overrides:
//
//	proxy: traefik
//
//	proxy:
//	  kind: traefik
//	  networks: [carimco-${ENV_NAME}]
//	  publish: ["8080:80"]
type ManagedProxy struct {
	// Name is the proxy's logical name within the stack (also its compose service
	// name and the default target of the stack's bindings). Defaults to
	// "internal".
	Name string `yaml:"name,omitempty"`
	// Kind selects the proxy implementation ("traefik", and later others). It
	// must be a kind kompensator can provision.
	Kind string `yaml:"kind"`
	// Networks are the docker networks the proxy joins to reach the services it
	// routes. They must already exist — declare them as stack (or env) Networks so
	// kompensator creates them before any project deploys. Names may reference
	// ${STACK_PREFIX}/${ENV_NAME}. Defaults to the stack's conventional network
	// "<stack>-${ENV_NAME}".
	Networks []ProxyNetwork `yaml:"networks,omitempty"`
	// Publish maps host ports onto the proxy, e.g. "8080:80". Empty means the
	// proxy is only reachable in-cluster (e.g. chained behind a user-managed edge
	// proxy that joins one of its networks).
	Publish []string `yaml:"publish,omitempty"`
}

// UnmarshalYAML accepts either a bare kind (scalar, "traefik") or a mapping of
// overrides, so both forms in stack.yml decode into a ManagedProxy.
func (m *ManagedProxy) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&m.Kind)
	}
	type raw ManagedProxy
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*m = ManagedProxy(r)
	return nil
}

// ProxyNetwork is one docker network a managed proxy joins, with optional
// aliases so another proxy can reach it under a stable name. The network must
// already exist — declare it as a stack/env Network so kompensator creates it
// before any project deploys. It decodes from a bare network name (scalar) or a
// mapping {name, aliases}.
type ProxyNetwork struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases,omitempty"`
}

// UnmarshalYAML accepts a scalar network name or a {name, aliases} mapping.
func (n *ProxyNetwork) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&n.Name)
	}
	type raw ProxyNetwork
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*n = ProxyNetwork(r)
	return nil
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
	// Proxy binds the project's Blue/Green switch to an environment reverse
	// proxy: after the new color is healthy the reconcile points the proxy at
	// it (see internal/proxy). A project may expose several services (e.g. a
	// frontend and a backend), so it can declare one binding per routed service.
	// Only meaningful for Blue/Green projects.
	Proxy []ProxyBinding `yaml:"proxy,omitempty"`
}

// ProxyBinding declares which reverse proxy fronts a Blue/Green project and how
// to route to it. It is proxy-agnostic; Kind selects the implementation.
type ProxyBinding struct {
	// Kind selects the proxy implementation ("traefik", and later "haproxy",
	// "nginx", ...).
	Kind string `yaml:"kind"`
	// Router is the logical route name, unique within the environment's proxy.
	// Defaults to the project name when empty.
	Router string `yaml:"router,omitempty"`
	// Service is the compose service to route to; Port is its container port.
	// The active color is reached on the shared network as "<service>-<color>".
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	// Rule is the routing rule in Traefik syntax (e.g. PathPrefix(`/`)); empty
	// matches everything. EntryPoint is the proxy listener name (proxy default
	// when empty).
	Rule       string `yaml:"rule,omitempty"`
	EntryPoint string `yaml:"entryPoint,omitempty"`
	// Target names the proxy that serves this route. It defaults to the stack's
	// managed proxy ("internal"). Naming a different proxy is reserved for an
	// advanced, user-managed setup.
	Target string `yaml:"target,omitempty"`
}

// RouterName returns the binding's router name, defaulting to the project name.
func (b ProxyBinding) RouterName(project string) string {
	if b.Router != "" {
		return b.Router
	}
	return project
}

// TargetName returns the proxy this binding routes through, defaulting to the
// stack's managed proxy.
func (b ProxyBinding) TargetName() string {
	if b.Target != "" {
		return b.Target
	}
	return defaultProxyName
}

// BlueGreen reports whether the project uses the Blue/Green strategy. Any value
// other than "recreate" (including the empty default) means Blue/Green.
func (p Project) BlueGreen() bool {
	return !strings.EqualFold(strings.TrimSpace(p.Strategy), StrategyRecreate)
}

// MergeVariables returns the effective variables from a set of layers: each
// layer overlays the previous one, so later arguments win. The reconciler feeds
// the scopes broad to narrow (stack defaults, env, env+node, stack placement,
// stack placement+node, project placement, project placement+node), so the
// narrowest scope wins and, within a scope, the per-node layer wins over the
// all-nodes one. No input is mutated; the result is always non-nil.
func MergeVariables(layers ...map[string]string) map[string]string {
	merged := make(map[string]string)
	for _, layer := range layers {
		for k, v := range layer {
			merged[k] = v
		}
	}
	return merged
}

// ResolveVariables computes the effective declared variables for one project of
// one stack on one node, applying the nested scopes broad to narrow (each layer
// overrides the previous):
//
//	stack defaults (stack.yml)
//	  < env.variables < env.nodeVariables[node]
//	  < placement.variables < placement.nodeVariables[node]
//	  < project.variables < project.nodeVariables[node]
//
// Within a scope the per-node layer wins over the all-nodes one, and a narrower
// scope wins over a broader one — so an inner all-nodes value still beats an
// outer per-node value. Secrets and identity built-ins are layered on top by
// the caller and are not part of this result.
func ResolveVariables(stackDefaults map[string]string, env Environment, placement StackPlacement, project, node string) map[string]string {
	proj := placement.Project(project)
	return MergeVariables(
		stackDefaults,
		env.Variables,
		env.NodeVars(node),
		placement.Variables,
		placement.NodeVars(node),
		proj.Variables,
		proj.NodeVars(node),
	)
}

// ServiceImage is the desired image and tag for a single service.
type ServiceImage struct {
	Image string `yaml:"image"`
	Tag   string `yaml:"tag"`
	// OneShot marks a job container that runs to completion and exits (compose
	// restart: "no"), e.g. a one-shot that publishes assets into a volume. Its
	// image/tag is still injected and folded into the deploy fingerprint (so a
	// new tag triggers a redeploy that re-runs it), but it is excluded from the
	// running-image drift check, which would otherwise see the exited container
	// as perpetually missing and redeploy on every reconcile.
	OneShot bool `yaml:"oneShot,omitempty"`
}

// Ref returns the fully qualified image reference (image:tag).
func (s ServiceImage) Ref() string {
	return s.Image + ":" + s.Tag
}

// StackState is the desired state for one stack in one environment, stored at
// environments/<env>/state/<stack>.yml. It maps project name -> service name ->
// desired image/tag.
type StackState map[string]map[string]ServiceImage

// LoadInventory reads inventory/nodes.yml from the checked-out repo root. A
// repo that has no inventory yet has no nodes; the file is written by the first
// 'node add'.
func LoadInventory(repoRoot string) (Inventory, error) {
	var inv Inventory
	if err := loadYAML(InventoryPath(repoRoot), &inv); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Inventory{}, nil
		}
		return Inventory{}, err
	}
	return inv, nil
}

// InventoryPath is the inventory file's location inside a deployment repo.
func InventoryPath(repoRoot string) string {
	return filepath.Join(repoRoot, "inventory", "nodes.yml")
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

	path := InventoryPath(repoRoot)
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
	if err := loadYAML(EnvironmentFile(repoRoot, env), &e); err != nil {
		return Environment{}, err
	}
	if e.Name == "" {
		e.Name = env
	}
	return e, nil
}

// EnvironmentDir returns the absolute path to an environment's directory.
func EnvironmentDir(repoRoot, env string) string {
	return filepath.Join(repoRoot, "environments", env)
}

// EnvironmentFile returns the absolute path to an environment's definition.
func EnvironmentFile(repoRoot, env string) string {
	return filepath.Join(EnvironmentDir(repoRoot, env), "env.yml")
}

// ListEnvironments returns the names of every environment defined in the repo
// (every environments/<env>/ directory that holds an env.yml), sorted. It is
// the source of truth for which environments exist, since nodes no longer carry
// environment membership.
func ListEnvironments(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, "environments")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read environments dir: %w", err)
	}
	var envs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "env.yml")); err != nil {
			continue
		}
		envs = append(envs, e.Name())
	}
	sort.Strings(envs)
	return envs, nil
}

// StackDir returns the absolute path to a stack's definition directory.
func StackDir(repoRoot, stack string) string {
	return filepath.Join(repoRoot, "stacks", stack)
}

// StackFile returns the absolute path to a stack's definition.
func StackFile(repoRoot, stack string) string {
	return filepath.Join(StackDir(repoRoot, stack), "stack.yml")
}

// ListStacks returns the names of every stack defined in the repo (every
// stacks/<name>/ directory that holds a stack.yml), sorted.
func ListStacks(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, "stacks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read stacks dir: %w", err)
	}
	var stacks []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "stack.yml")); err != nil {
			continue
		}
		stacks = append(stacks, e.Name())
	}
	sort.Strings(stacks)
	return stacks, nil
}

// EnvVarName converts a service name to the uppercase env-var prefix under
// which kompensator injects its image and tag, e.g. "frontend" -> "FRONTEND",
// "apk-distribution" -> "APK_DISTRIBUTION".
func EnvVarName(service string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, service)
}

// LoadStack reads stacks/<stack>/stack.yml.
func LoadStack(repoRoot, stack string) (Stack, error) {
	var s Stack
	if err := loadYAML(StackFile(repoRoot, stack), &s); err != nil {
		return Stack{}, err
	}
	if s.Name == "" {
		s.Name = stack
	}
	if s.Proxy != nil {
		if s.Proxy.Name == "" {
			s.Proxy.Name = defaultProxyName
		}
		if len(s.Proxy.Networks) == 0 {
			s.Proxy.Networks = []ProxyNetwork{{Name: s.Name + "-${ENV_NAME}"}}
		}
	}
	for i := range s.Projects {
		if s.Projects[i].Strategy == "" {
			s.Projects[i].Strategy = StrategyBlueGreen
		}
		// A binding inherits the stack's managed proxy kind when it does not name
		// one, so the common case needs no per-binding "kind".
		if s.Proxy != nil {
			for j := range s.Projects[i].Proxy {
				if s.Projects[i].Proxy[j].Kind == "" {
					s.Projects[i].Proxy[j].Kind = s.Proxy.Kind
				}
			}
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

// StateFile returns the absolute path to a stack's desired-state file for an
// environment: environments/<env>/state/<stack>.yml.
func StateFile(repoRoot, env, stack string) string {
	return filepath.Join(EnvironmentDir(repoRoot, env), "state", stack+".yml")
}

// LoadStackState reads environments/<env>/state/<stack>.yml as the desired
// state for the stack. A missing file yields an empty state (no desired images
// yet), not an error.
func LoadStackState(repoRoot, env, stack string) (StackState, error) {
	path := StateFile(repoRoot, env, stack)
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
