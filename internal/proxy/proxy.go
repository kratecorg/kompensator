// Package proxy decouples kompensator's Blue/Green color switch from any
// particular reverse proxy. A Blue/Green reconcile deploys the new color, waits
// for it to become healthy, and then must point the environment's reverse proxy
// at that color. That last step is the only thing that differs between proxies
// (Traefik, HAProxy, nginx, ...), so it lives behind the Router interface.
//
// The design is deliberately pluggable: reconcile knows nothing about Traefik.
// It looks up a Router by kind from the registry and calls Switch. A new proxy
// is added by implementing Router and calling Register from an init function —
// no change to the reconcile logic is needed.
package proxy

import (
	"context"
	"fmt"
	"sort"
)

// Target describes the route a Router must (re)point at a Blue/Green color. It
// carries everything a proxy needs to render its own configuration, but nothing
// proxy-specific, so every implementation consumes the same input.
type Target struct {
	// Env, Stack and Project identify the deployment the route belongs to.
	Env     string
	Stack   string
	Project string

	// Router is the logical route name. An implementation typically uses it as
	// the name of the config object it writes (e.g. the Traefik router/service
	// name and the dynamic file name), so it must be unique within DynamicDir.
	Router string

	// Service is the compose service to route to and Port its container port.
	// The active color's container is reachable on the shared environment
	// network under the DNS alias "<Service>-<Color>" (the compose file is
	// expected to publish that alias), so a Router targets host
	// "<Service>-<Color>:<Port>".
	Service string
	Port    int

	// Servers, when non-empty, lists the concrete backend hosts the Router
	// should balance across — one per replica of the active color. kompensator
	// resolves them from the running containers (as IP addresses, which avoid
	// the 63-octet DNS-label limit a long container name can exceed) so a
	// file-based proxy load-balances real replicas instead of pinning to a
	// single round-robin DNS name. When empty the Router falls back to the
	// single alias "<Service>-<Color>".
	Servers []string

	// Rule is the proxy-agnostic routing rule expressed in Traefik syntax
	// (e.g. `PathPrefix(`+"`/`"+`)`). Implementations that are not Traefik
	// translate it; an empty Rule means "match everything".
	Rule string

	// Color is the Blue/Green slot that should now receive traffic.
	Color string

	// EntryPoint is the proxy entrypoint/listener name to bind the route to
	// (e.g. Traefik "web"). Empty means the implementation's default.
	EntryPoint string

	// DynamicDir is the node-local directory a file-based proxy watches for
	// dynamic configuration. The Router writes its file here; the proxy
	// container bind-mounts the same directory.
	DynamicDir string
}

// Validate checks that the fields every Router relies on are set.
func (t Target) Validate() error {
	switch {
	case t.Router == "":
		return fmt.Errorf("proxy target: router is required")
	case t.Service == "":
		return fmt.Errorf("proxy target: service is required")
	case t.Port == 0:
		return fmt.Errorf("proxy target: port is required")
	case t.Color == "" && len(t.Servers) == 0:
		// Color names the fallback backend alias "<Service>-<Color>"; it is only
		// required when no concrete Servers were resolved. A recreate project has
		// no color but always supplies its running containers as Servers.
		return fmt.Errorf("proxy target: color or servers required")
	case t.DynamicDir == "":
		return fmt.Errorf("proxy target: dynamic dir is required")
	}
	return nil
}

// Router points an environment's reverse proxy at the active Blue/Green color.
// Implementations must make Switch atomic (a partially written config must
// never be observed) and idempotent (switching to the color already served is a
// no-op that still repairs a missing or stale config file).
type Router interface {
	// Kind is the proxy identifier used in the stack definition ("traefik",
	// "haproxy", "nginx", ...).
	Kind() string
	// Switch makes the proxy route Target to Target.Color.
	Switch(ctx context.Context, t Target) error
}

// ManagedSpec describes a stack-internal proxy that kompensator runs on the
// stack's behalf. A Provisioner turns it into a compose file kompensator
// deploys as an ordinary recreate project; the proxy then watches DynamicDir
// for the routes written on each Blue/Green switch. Only ENV_NAME is available
// to the rendered compose file, so the proxy is immune to stack/env variable
// churn — kompensator controls everything it references.
type ManagedSpec struct {
	// Name is the proxy's compose service name within its synthesized project.
	Name string
	// Env and Stack identify the deployment the proxy belongs to.
	Env   string
	Stack string
	// DynamicDir is the node-local host path the proxy bind-mounts read-only as
	// its dynamic-config watch directory (the same dir Routers write into).
	DynamicDir string
	// Networks are the docker networks to join (external; names may contain
	// ${ENV_NAME}), each with optional aliases for chaining from another proxy.
	Networks []ManagedNetwork
	// Publish maps host ports onto the proxy, e.g. "8080:80". Empty = in-cluster
	// only.
	Publish []string
}

// ManagedNetwork is one docker network a managed proxy attaches to. By default
// it is joined as external (it must already exist); when Owned is set the proxy
// creates the network itself with the exact name given.
type ManagedNetwork struct {
	Name    string
	Aliases []string
	Owned   bool
}

// Provisioner is implemented by a proxy kind kompensator can run on a stack's
// behalf (no hand-written compose file). Compose renders a complete
// docker-compose file for the proxy described by spec.
type Provisioner interface {
	Compose(spec ManagedSpec) ([]byte, error)
}

// factory builds a Router of one kind.
type factory func() (Router, error)

// registry maps a proxy kind to its factory. Implementations register
// themselves from an init function.
var registry = map[string]factory{}

// Register adds a Router factory under its kind. It panics on a duplicate kind,
// which can only be a programming error (two implementations claiming the same
// name). Call it from an init function.
func Register(kind string, f factory) {
	if _, dup := registry[kind]; dup {
		panic("proxy: duplicate Router kind " + kind)
	}
	registry[kind] = f
}

// New returns a Router for the given kind, or an error listing the supported
// kinds when none matches.
func New(kind string) (Router, error) {
	f, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown proxy kind %q (supported: %v)", kind, Kinds())
	}
	return f()
}

// Kinds returns the registered proxy kinds, sorted.
func Kinds() []string {
	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}
