package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Label keys kompensator stamps onto every service container it deploys. They
// let a later reconcile identify kompensator-managed containers precisely (by
// exact label match rather than by parsing the compose project name) and, above
// all, tell them apart from foreign containers. Only a container carrying
// LabelManaged may be torn down by an automated prune; anything without it is
// treated as foreign and left untouched.
const (
	LabelManaged = "kompensator.managed"
	LabelRepo    = "kompensator.repo"
	LabelNode    = "kompensator.node"
	LabelEnv     = "kompensator.env"
	LabelStack   = "kompensator.stack"
	LabelProject = "kompensator.project"
	LabelColor   = "kompensator.color"
)

// Labels is the kompensator identity applied to every service container of a
// deploy. The managed marker is always set; Color is empty for recreate and
// managed-proxy projects.
type Labels struct {
	Repo    string
	Node    string
	Env     string
	Stack   string
	Project string
	Color   string
}

// pairs returns the label key/value pairs to stamp on every service container.
// LabelManaged is always present; the identity fields are included only when
// set, so an omitted optional segment (e.g. Color) leaves no empty label behind.
func (l Labels) pairs() [][2]string {
	out := [][2]string{{LabelManaged, "true"}}
	add := func(key, value string) {
		if value != "" {
			out = append(out, [2]string{key, value})
		}
	}
	add(LabelRepo, l.Repo)
	add(LabelNode, l.Node)
	add(LabelEnv, l.Env)
	add(LabelStack, l.Stack)
	add(LabelProject, l.Project)
	add(LabelColor, l.Color)
	return out
}

// Colors are the two Blue/Green deployment slots. A Blue/Green reconcile always
// deploys the new version into the slot the project is not currently running
// in, then stops the old slot once the new one is healthy.
const (
	ColorBlue  = "blue"
	ColorGreen = "green"
)

// Colors lists the Blue/Green slots in a stable order.
var Colors = []string{ColorBlue, ColorGreen}

// OtherColor returns the opposite Blue/Green slot. An empty (no color running)
// input maps to blue, so a first-ever deploy lands in blue.
func OtherColor(color string) string {
	if color == ColorBlue {
		return ColorGreen
	}
	return ColorBlue
}

// Names holds the segments that prefix a Docker Compose project name. Repo and
// Node are optional (controlled by config) and let an operator shorten names,
// e.g. drop the node segment when every node owns its own host. Node always
// carries the real node name because it is also injected as NODE_NAME at deploy
// time, independent of whether it appears in the project name.
type Names struct {
	Repo        string // deployment repo name
	Node        string // node name (always used as NODE_NAME at deploy time)
	IncludeRepo bool   // include Repo in the project name
	IncludeNode bool   // include Node in the project name
}

// leadingSegments returns the optional repo/node segments that are enabled.
func (n Names) leadingSegments() []string {
	var parts []string
	if n.IncludeRepo && n.Repo != "" {
		parts = append(parts, n.Repo)
	}
	if n.IncludeNode && n.Node != "" {
		parts = append(parts, n.Node)
	}
	return parts
}

// Project builds the Docker Compose project name for one project of a stack:
//
//	[<repo>-][<node>-]<env>-<stack>-<project>[-<color>]
//
// env, stack and project are always present; the color is appended for
// Blue/Green projects and omitted (empty color) for recreate projects.
func (n Names) Project(env, stack, project, color string) string {
	parts := append(n.leadingSegments(), env, stack, project)
	base := strings.Join(parts, "-")
	if color != "" {
		base += "-" + color
	}
	return sanitize(base)
}

// StackPrefix returns the name prefix shared by every project of a stack:
//
//	[<repo>-][<node>-]<env>-<stack>
//
// It is identical across all projects of the same stack (and stable across
// Blue/Green color switches), so it is the handle for naming stack-scoped
// resources one project owns and another references — e.g. a shared network.
func (n Names) StackPrefix(env, stack string) string {
	parts := append(n.leadingSegments(), env, stack)
	return sanitize(strings.Join(parts, "-"))
}

// TeardownPrefix returns the project-name prefix that matches every project of
// this node, or "" when neither repo nor node is part of the name (in which
// case projects cannot be scoped to a single node).
func (n Names) TeardownPrefix() string {
	parts := n.leadingSegments()
	if len(parts) == 0 {
		return ""
	}
	return sanitize(strings.Join(parts, "-")) + "-"
}

// Container is one running (or stopped) container of a project's service.
type Container struct {
	Color   string // "blue" / "green" / "" (recreate); set by the caller
	Service string // compose service name, e.g. "frontend"
	Name    string // short name, e.g. "frontend-1" (service + replica number)
	Image   string // image reference it runs
	Health  string // concise health: healthy/starting/unhealthy/running/exited
}

// ProjectImages reports the running image per service for a compose project, or
// an empty map when nothing runs. dockerHost is the docker "-H" endpoint to
// query ("" = local daemon); a controller passes a node's ssh:// endpoint.
func ProjectImages(ctx context.Context, dockerHost, project string) (map[string]string, error) {
	out, err := output(ctx, "docker", dockerArgs(dockerHost,
		"ps",
		"--filter", "label=com.docker.compose.project="+project,
		"--format", `{{.Label "com.docker.compose.service"}}`+"\t{{.Image}}",
	)...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}
	images := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		images[parts[0]] = parts[1]
	}
	return images, nil
}

// ServiceEndpoints returns one backend host per RUNNING container of a service
// in a compose project, sorted, for a reverse proxy to load-balance across.
//
// It returns container IP addresses, not names. Compose names a container
// "<project>-<service>-<n>", which a proxy on the same network could in
// principle resolve via docker's embedded DNS — but that name is a single DNS
// label and DNS labels are capped at 63 octets, so a long enough
// repo/node/env/stack/project combination produces a name the embedded DNS
// silently refuses to resolve. IP addresses sidestep that limit entirely and
// still address each replica directly (real per-replica load balancing).
// kompensator rewrites the proxy config on every deploy and re-asserts it on
// every in-sync reconcile, so the IPs never go stale for long. dockerHost is
// the docker "-H" endpoint ("" = local daemon).
//
// preferNetworks names the networks the requesting proxy is attached to. A
// service container may join several networks (e.g. an "app" network for the
// database and an "internal" network shared with the proxy); the proxy can only
// reach it on a network they share, so when preferNetworks is non-empty the IP
// on the first matching network is returned. When it is empty, or none match,
// the first network's IP is used (services historically join a single network).
func ServiceEndpoints(ctx context.Context, dockerHost, project, service string, preferNetworks ...string) ([]string, error) {
	out, err := output(ctx, "docker", dockerArgs(dockerHost,
		"ps",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.Names}}",
	)...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)

	// Resolve each container's IP. The template prints "<network>=<ip>" for
	// every network the container is attached to, space-separated. A service
	// may join multiple networks, so pick the IP on a network the proxy shares
	// (preferNetworks); fall back to the first network when none match.
	prefer := make(map[string]bool, len(preferNetworks))
	for _, n := range preferNetworks {
		if n != "" {
			prefer[n] = true
		}
	}
	args := append([]string{"inspect",
		"--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}"},
		names...)
	out, err = output(ctx, "docker", dockerArgs(dockerHost, args...)...)
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w: %s", err, out)
	}
	var ips []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ip := pickEndpointIP(fields, prefer)
		if ip == "" {
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) != len(names) {
		return nil, fmt.Errorf("resolved %d IP(s) for %d container(s) of service %q", len(ips), len(names), service)
	}
	return ips, nil
}

// pickEndpointIP selects a container's reachable IP from "<network>=<ip>"
// fields. It returns the IP on the first network present in prefer; failing
// that (or when prefer is empty) the first field's IP, preserving the previous
// single-network behaviour.
func pickEndpointIP(fields []string, prefer map[string]bool) string {
	first := ""
	for _, f := range fields {
		name, ip, ok := strings.Cut(f, "=")
		if !ok || ip == "" {
			continue
		}
		if first == "" {
			first = ip
		}
		if prefer[name] {
			return ip
		}
	}
	return first
}

// ProjectContainers lists every container of a compose project (running or
// stopped), one entry per replica, sorted by name. The short name is
// "<service>-<replica-number>". dockerHost is the docker "-H" endpoint ("" =
// local daemon). Color is left empty for the caller to fill in.
func ProjectContainers(ctx context.Context, dockerHost, project string) ([]Container, error) {
	const fmtStr = `{{.Label "com.docker.compose.service"}}` + "\t" +
		`{{.Label "com.docker.compose.service"}}-{{.Label "com.docker.compose.container-number"}}` +
		"\t{{.Image}}\t{{.Status}}"
	out, err := output(ctx, "docker", dockerArgs(dockerHost,
		"ps", "-a",
		"--filter", "label=com.docker.compose.project="+project,
		"--format", fmtStr,
	)...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}

	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			continue
		}
		cs = append(cs, Container{
			Service: parts[0],
			Name:    parts[1],
			Image:   parts[2],
			Health:  shortHealth(parts[3]),
		})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, nil
}

// shortHealth reduces docker's human status string (e.g. "Up 2 minutes
// (healthy)") to a single concise token for the status table.
func shortHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	case strings.HasPrefix(status, "Up"):
		return "running"
	case strings.HasPrefix(status, "Exited"), strings.HasPrefix(status, "Exit"):
		return "exited"
	case strings.HasPrefix(status, "Created"):
		return "created"
	case strings.HasPrefix(status, "Restarting"):
		return "restarting"
	default:
		return strings.ToLower(firstLine(status))
	}
}

// Deploy brings a compose project up to its desired state using Docker Compose.
// The compose file references per-service variables (e.g. ${FRONTEND_IMAGE},
// ${FRONTEND_TAG}) and may use ${NODE_NAME}; extraEnv carries those values as
// "KEY=value" entries.
//
// Images are pulled by compose only when missing. The GitOps model uses
// immutable tags (new version = new tag), so a missing-pull is sufficient and
// also lets locally-built images work without a registry.
//
// Deploy is only called once kompensator has decided a project must be
// (re)deployed (image drift, config-hash change or --force). It therefore runs
// with --force-recreate so changes Compose would not notice on its own — most
// importantly the contents of bind-mounted config files such as haproxy.cfg —
// are actually applied. On the in-sync path Deploy is not called at all, so
// this never churns healthy containers needlessly.
//
// Deploy stamps kompensator's identity labels (see Labels) onto every service
// via a generated override compose file merged with -f, so later reconciles can
// recognise the containers as kompensator-managed. --force-recreate guarantees
// the labels are (re)applied even when nothing else changed.
func Deploy(ctx context.Context, composeFile, project string, labels Labels, extraEnv []string) error {
	env := append(os.Environ(), "NODE_NAME="+labels.Node)
	env = append(env, extraEnv...)

	services, err := composeServices(ctx, composeFile, env)
	if err != nil {
		return err
	}
	overrideFile, err := writeLabelOverride(services, labels)
	if err != nil {
		return fmt.Errorf("write label override (%s): %w", project, err)
	}
	defer os.Remove(overrideFile)

	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-p", project,
		"-f", composeFile,
		"-f", overrideFile,
		"up", "-d", "--remove-orphans", "--force-recreate",
	)
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up (%s): %w", project, err)
	}
	return nil
}

// composeServices returns the service names a compose file defines, resolved
// with the deploy env so variable-interpolated and profile-gated files list
// exactly the services `up` will create. It is the set a label override must
// enumerate to stamp a label on every container.
func composeServices(ctx context.Context, composeFile string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "config", "--services")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config --services (%s): %w: %s", composeFile, err, strings.TrimSpace(stderr.String()))
	}
	var services []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			services = append(services, s)
		}
	}
	return services, nil
}

// writeLabelOverride writes a compose override file that stamps the kompensator
// identity labels onto every given service and returns its path; the caller
// removes it after the deploy. Merged with -f after the base file, its labels
// are added to whatever each service already declares.
func writeLabelOverride(services []string, labels Labels) (string, error) {
	var b strings.Builder
	b.WriteString("services:\n")
	for _, service := range services {
		fmt.Fprintf(&b, "  %q:\n    labels:\n", service)
		for _, kv := range labels.pairs() {
			fmt.Fprintf(&b, "      %q: %q\n", kv[0], kv[1])
		}
	}
	f, err := os.CreateTemp("", "kompensator-labels-*.yml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// EnsureNetwork creates the named docker network if it does not already exist,
// so projects can join it as external without any project having to "own" it
// via compose. It is idempotent: an existing network (or a concurrent create
// that lost the race) is treated as success. Returns true when it created the
// network. host is the docker "-H" endpoint ("" = local daemon).
func EnsureNetwork(ctx context.Context, host, name, driver string, options map[string]string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("ensure network: empty name")
	}
	if _, err := output(ctx, "docker", dockerArgs(host, "network", "inspect", name)...); err == nil {
		return false, nil
	}
	args := []string{"network", "create"}
	if driver != "" {
		args = append(args, "--driver", driver)
	}
	for k, v := range options {
		args = append(args, "--opt", k+"="+v)
	}
	args = append(args, name)
	if out, err := output(ctx, "docker", dockerArgs(host, args...)...); err != nil {
		if strings.Contains(out, "already exists") {
			return false, nil
		}
		return false, fmt.Errorf("docker network create %s: %w: %s", name, err, out)
	}
	return true, nil
}

// EnsureVolume creates the named docker volume if it does not already exist, so
// a project can reference it as external instead of having compose create a
// project-scoped one. Idempotent; returns true when it created the volume. host
// is the docker "-H" endpoint ("" = local daemon).
func EnsureVolume(ctx context.Context, host, name, driver string, options map[string]string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("ensure volume: empty name")
	}
	if _, err := output(ctx, "docker", dockerArgs(host, "volume", "inspect", name)...); err == nil {
		return false, nil
	}
	args := []string{"volume", "create"}
	if driver != "" {
		args = append(args, "--driver", driver)
	}
	for k, v := range options {
		args = append(args, "--opt", k+"="+v)
	}
	args = append(args, name)
	if out, err := output(ctx, "docker", dockerArgs(host, args...)...); err != nil {
		if strings.Contains(out, "already exists") {
			return false, nil
		}
		return false, fmt.Errorf("docker volume create %s: %w: %s", name, err, out)
	}
	return true, nil
}

// Stop tears down a compose project (the old color after a successful switch).
func Stop(ctx context.Context, project string) error {
	out, err := output(ctx, "docker", "compose", "-p", project, "down")
	if err != nil {
		return fmt.Errorf("docker compose down (%s): %w: %s", project, err, out)
	}
	return nil
}

// Down tears down a compose project on a specific docker host ("" = local
// daemon). Used when removing a node, to stop all of its projects.
func Down(ctx context.Context, host, project string) error {
	out, err := output(ctx, "docker", dockerArgs(host, "compose", "-p", project, "down")...)
	if err != nil {
		return fmt.Errorf("docker compose down (%s): %w: %s", project, err, out)
	}
	return nil
}

// ListProjects returns the distinct compose project names whose name starts
// with prefix, on the given docker host ("" = local daemon). Used to find every
// project belonging to a node (see Names.TeardownPrefix).
func ListProjects(ctx context.Context, host, prefix string) ([]string, error) {
	out, err := output(ctx, "docker", dockerArgs(host,
		"ps", "-a",
		"--format", `{{.Label "com.docker.compose.project"}}`,
	)...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}
	seen := map[string]bool{}
	var projects []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || !strings.HasPrefix(p, prefix) || seen[p] {
			continue
		}
		seen[p] = true
		projects = append(projects, p)
	}
	sort.Strings(projects)
	return projects, nil
}

// ManagedProject is one compose project kompensator created, identified by the
// kompensator.* labels it stamps on every container. Because it is discovered
// via LabelManaged, anything ListManagedProjects returns is kompensator's own —
// foreign projects never appear.
type ManagedProject struct {
	Name    string // docker compose project name
	Env     string
	Stack   string
	Project string
	Color   string // Blue/Green slot, or "" (recreate / managed proxy)
}

// ListManagedProjects returns the distinct kompensator-managed compose projects
// on host, scoped to a repo and node and, when env is non-empty, to that env.
// It filters on LabelManaged so only kompensator's own containers are ever
// considered — the caller may safely act on the result without risking a
// foreign container. host is the docker "-H" endpoint ("" = local daemon).
func ListManagedProjects(ctx context.Context, host, repo, node, env string) ([]ManagedProject, error) {
	args := []string{"ps", "-a", "--filter", "label=" + LabelManaged + "=true"}
	if repo != "" {
		args = append(args, "--filter", "label="+LabelRepo+"="+repo)
	}
	if node != "" {
		args = append(args, "--filter", "label="+LabelNode+"="+node)
	}
	if env != "" {
		args = append(args, "--filter", "label="+LabelEnv+"="+env)
	}
	const fmtStr = `{{.Label "com.docker.compose.project"}}` + "\t" +
		`{{.Label "` + LabelEnv + `"}}` + "\t" +
		`{{.Label "` + LabelStack + `"}}` + "\t" +
		`{{.Label "` + LabelProject + `"}}` + "\t" +
		`{{.Label "` + LabelColor + `"}}`
	args = append(args, "--format", fmtStr)

	out, err := output(ctx, "docker", dockerArgs(host, args...)...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}
	return parseManagedProjects(out), nil
}

// parseManagedProjects turns the tab-separated `docker ps` output of
// ListManagedProjects into distinct projects. It splits on newlines only and
// never trims the lines with TrimSpace: the last field (color) is empty for
// recreate and managed-proxy projects, so the line ends in a tab that TrimSpace
// would eat — dropping a field and, with it, the whole row.
func parseManagedProjects(out string) []ManagedProject {
	seen := map[string]bool{}
	var projects []ManagedProject
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			continue
		}
		name := parts[0]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		projects = append(projects, ManagedProject{
			Name: name, Env: parts[1], Stack: parts[2], Project: parts[3], Color: parts[4],
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}

// WaitHealthy blocks until every container of the project reports healthy, or
// the timeout elapses. Containers without a healthcheck count as healthy as
// soon as they are running, so plain images deploy without extra config while
// images that define a HEALTHCHECK gate the Blue/Green switch.
func WaitHealthy(ctx context.Context, project string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ids, err := containerIDs(ctx, project)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("no containers found for project %q", project)
		}

		allHealthy := true
		for _, id := range ids {
			healthy, err := containerHealthy(ctx, id)
			if err != nil {
				return err
			}
			if !healthy {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for project %q to become healthy", timeout, project)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// containerIDs lists the container IDs belonging to a compose project.
func containerIDs(ctx context.Context, project string) ([]string, error) {
	out, err := output(ctx, "docker", "ps", "-q",
		"--filter", "label=com.docker.compose.project="+project,
	)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w: %s", err, out)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

// containerHealthy reports whether a container is healthy. A container that
// defines no healthcheck is healthy as soon as it is running.
func containerHealthy(ctx context.Context, id string) (bool, error) {
	out, err := output(ctx, "docker", "inspect",
		"--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none:{{.State.Running}}{{end}}",
		id,
	)
	if err != nil {
		return false, fmt.Errorf("docker inspect %s: %w: %s", id, err, out)
	}
	status := firstLine(out)
	return status == "healthy" || status == "none:true", nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// dockerArgs prepends "-H <host>" to a docker invocation when host is set, so
// the same command can target a local or a remote (ssh://) daemon.
func dockerArgs(host string, args ...string) []string {
	if host == "" {
		return args
	}
	return append([]string{"-H", host}, args...)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
