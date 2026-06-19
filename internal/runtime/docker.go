package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Colors are the two Blue/Green deployment slots. A reconcile always deploys
// the new version into the slot the app is not currently running in, then
// stops the old slot once the new one is healthy.
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

// ProjectName builds a Docker Compose project name for one Blue/Green slot. It
// includes the node name so that multiple simulated nodes can run on a single
// host during local testing without colliding. On a real one-node-per-host
// setup the node name is simply a stable prefix.
func ProjectName(node, env, app, color string) string {
	return sanitize(fmt.Sprintf("kompensator-%s-%s-%s-%s", node, env, app, color))
}

// ColorState is a running Blue/Green slot and the image it serves.
type ColorState struct {
	Color string
	Image string
}

// RunningColors reports which Blue/Green slots currently have a running
// container for the app's service, together with the image each one serves.
// The result has at most two entries (blue, green) and is empty when nothing
// is running.
func RunningColors(ctx context.Context, node, env, app, service string) ([]ColorState, error) {
	var states []ColorState
	for _, color := range Colors {
		project := ProjectName(node, env, app, color)
		image, err := RunningImage(ctx, project, service)
		if err != nil {
			return nil, err
		}
		if image != "" {
			states = append(states, ColorState{Color: color, Image: image})
		}
	}
	return states, nil
}

// RunningImage returns the image reference of the running container for the
// given compose project/service, or "" if none is running.
func RunningImage(ctx context.Context, project, service string) (string, error) {
	out, err := output(ctx, "docker", "ps",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.Image}}",
	)
	if err != nil {
		return "", fmt.Errorf("docker ps: %w: %s", err, out)
	}
	return firstLine(out), nil
}

// Deploy brings one Blue/Green slot up to the desired image/tag using Docker
// Compose. The compose file is expected to reference ${IMAGE} and ${TAG}, and
// may use ${NODE_NAME}.
//
// Images are pulled by compose only when missing. The GitOps model uses
// immutable tags (new version = new tag), so a missing-pull is sufficient and
// also lets locally-built images work without a registry.
func Deploy(ctx context.Context, composeFile, project, node, image, tag string) error {
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-p", project,
		"-f", composeFile,
		"up", "-d", "--remove-orphans",
	)
	cmd.Env = append(os.Environ(),
		"IMAGE="+image,
		"TAG="+tag,
		"NODE_NAME="+node,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up (%s): %w", project, err)
	}
	return nil
}

// Stop tears down a Blue/Green slot (the old color after a successful switch).
func Stop(ctx context.Context, project string) error {
	out, err := output(ctx, "docker", "compose", "-p", project, "down")
	if err != nil {
		return fmt.Errorf("docker compose down (%s): %w: %s", project, err, out)
	}
	return nil
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
