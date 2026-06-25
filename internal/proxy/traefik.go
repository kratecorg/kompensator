package proxy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// traefikEntryPoint is the entrypoint a route binds to when Target.EntryPoint
// is empty. It must match an entrypoint defined in the Traefik static config
// (the demo proxy stack defines "web").
const traefikEntryPoint = "web"

// traefikImage is the image kompensator runs for a stack-internal (managed)
// Traefik. The static config is supplied entirely on the command line so no
// Docker provider or socket is needed.
const traefikImage = "traefik:v3.1"

func init() {
	Register("traefik", func() (Router, error) { return traefik{}, nil })
}

// traefik is a Router for the Traefik file provider. On Switch it writes one
// dynamic configuration file per route into the watched directory; Traefik
// reloads it automatically, so the color switch needs no container restart.
type traefik struct{}

func (traefik) Kind() string { return "traefik" }

// traefikDynamic is the subset of Traefik's dynamic (file provider) schema we
// generate: a single router bound to an entrypoint, and the service it points
// at, whose only server is the active color's container.
type traefikDynamic struct {
	HTTP traefikHTTP `yaml:"http"`
}

type traefikHTTP struct {
	Routers  map[string]traefikRouter  `yaml:"routers"`
	Services map[string]traefikService `yaml:"services"`
}

type traefikRouter struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
}

type traefikService struct {
	LoadBalancer traefikLoadBalancer `yaml:"loadBalancer"`
}

type traefikLoadBalancer struct {
	Servers        []traefikServer `yaml:"servers"`
	PassHostHeader bool            `yaml:"passHostHeader"`
}

type traefikServer struct {
	URL string `yaml:"url"`
}

// Switch renders the dynamic file that routes Target.Router to the active
// color's container and writes it atomically into Target.DynamicDir.
func (traefik) Switch(ctx context.Context, t Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	rule := t.Rule
	if rule == "" {
		rule = "PathPrefix(`/`)"
	}
	entryPoint := t.EntryPoint
	if entryPoint == "" {
		entryPoint = traefikEntryPoint
	}

	// Prefer the concrete per-replica backends kompensator resolved so Traefik
	// load-balances across all replicas of the active color. Fall back to the
	// single "<service>-<color>" alias when none were given.
	hosts := t.Servers
	if len(hosts) == 0 {
		hosts = []string{fmt.Sprintf("%s-%s", t.Service, t.Color)}
	}
	servers := make([]traefikServer, len(hosts))
	for i, h := range hosts {
		servers[i] = traefikServer{URL: fmt.Sprintf("http://%s:%d", h, t.Port)}
	}

	doc := traefikDynamic{HTTP: traefikHTTP{
		Routers: map[string]traefikRouter{
			t.Router: {Rule: rule, EntryPoints: []string{entryPoint}, Service: t.Router},
		},
		Services: map[string]traefikService{
			t.Router: {LoadBalancer: traefikLoadBalancer{
				Servers:        servers,
				PassHostHeader: true,
			}},
		},
	}}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Managed by kompensator. Route %s/%s/%s -> color %s.\n", t.Env, t.Stack, t.Project, t.Color)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode traefik dynamic config: %w", err)
	}
	enc.Close()

	return writeFileAtomic(filepath.Join(t.DynamicDir, t.Router+".yml"), buf.Bytes())
}

// Compose renders the docker-compose file for a stack-internal Traefik. The
// proxy runs the file provider only: its whole static config is on the command
// line, it bind-mounts the dynamic-config directory read-only, and it joins the
// stack's network(s) so it can reach the service replicas kompensator lists as
// backends. kompensator deploys the result as a recreate project.
func (traefik) Compose(spec ManagedSpec) ([]byte, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("managed traefik: name is required")
	}
	if spec.DynamicDir == "" {
		return nil, fmt.Errorf("managed traefik: dynamic dir is required")
	}
	if len(spec.Networks) == 0 {
		return nil, fmt.Errorf("managed traefik: at least one network is required")
	}

	svcNets := make(map[string]traefikComposeSvcNet, len(spec.Networks))
	topNets := make(map[string]traefikComposeNetwork, len(spec.Networks))
	for i, n := range spec.Networks {
		key := fmt.Sprintf("net%d", i)
		svcNets[key] = traefikComposeSvcNet{Aliases: n.Aliases}
		// All networks are external: kompensator's resource phase creates them
		// before any project deploys, so the proxy only ever joins them.
		topNets[key] = traefikComposeNetwork{External: true, Name: n.Name}
	}

	doc := traefikCompose{
		Services: map[string]traefikComposeService{
			spec.Name: {
				Image:   traefikImage,
				Restart: "unless-stopped",
				Command: []string{
					"--providers.file.directory=/dynamic",
					"--providers.file.watch=true",
					"--entryPoints." + traefikEntryPoint + ".address=:80",
					"--log.level=INFO",
				},
				Volumes:  []string{spec.DynamicDir + ":/dynamic:ro"},
				Networks: svcNets,
				Ports:    spec.Publish,
			},
		},
		Networks: topNets,
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Managed by kompensator \u2014 stack-internal traefik for %s/%s. Do not edit.\n", spec.Env, spec.Stack)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode managed traefik compose: %w", err)
	}
	enc.Close()
	return buf.Bytes(), nil
}

// traefikCompose is the subset of the compose schema we generate for a managed
// Traefik: one service joined to one or more external networks.
type traefikCompose struct {
	Services map[string]traefikComposeService `yaml:"services"`
	Networks map[string]traefikComposeNetwork `yaml:"networks"`
}

type traefikComposeService struct {
	Image    string                          `yaml:"image"`
	Restart  string                          `yaml:"restart"`
	Command  []string                        `yaml:"command"`
	Volumes  []string                        `yaml:"volumes"`
	Networks map[string]traefikComposeSvcNet `yaml:"networks"`
	Ports    []string                        `yaml:"ports,omitempty"`
}

type traefikComposeSvcNet struct {
	Aliases []string `yaml:"aliases,omitempty"`
}

type traefikComposeNetwork struct {
	External bool   `yaml:"external"`
	Name     string `yaml:"name"`
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory followed by a rename, so a watcher never observes a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
