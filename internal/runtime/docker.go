package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ProjectName builds a Docker Compose project name. It includes the node name
// so that multiple simulated nodes can run on a single host during local
// testing without colliding. On a real one-node-per-host setup the node name
// is simply a stable prefix.
func ProjectName(node, env, app string) string {
	return sanitize(fmt.Sprintf("kompensator-%s-%s-%s", node, env, app))
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

// Deploy brings the app up to the desired image/tag using Docker Compose.
// Phase 1 uses a plain recreate (compose up -d). The compose file is expected
// to reference ${IMAGE} and ${TAG}, and may use ${NODE_NAME}.
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
