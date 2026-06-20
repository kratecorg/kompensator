package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

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

// ProjectName builds a Docker Compose project name for one project of a stack.
// It includes the node name so that multiple simulated nodes can run on a
// single host during local testing without colliding. For Blue/Green projects
// the color is appended; recreate projects pass an empty color and get no
// suffix.
func ProjectName(node, env, stack, project, color string) string {
	base := fmt.Sprintf("kompensator-%s-%s-%s-%s", node, env, stack, project)
	if color != "" {
		base += "-" + color
	}
	return sanitize(base)
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
func Deploy(ctx context.Context, composeFile, project, node string, extraEnv []string) error {
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-p", project,
		"-f", composeFile,
		"up", "-d", "--remove-orphans",
	)
	cmd.Env = append(os.Environ(), "NODE_NAME="+node)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up (%s): %w", project, err)
	}
	return nil
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
// project belonging to a node (prefix "kompensator-<node>-").
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
