package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"kompensator/internal/repo"
	"kompensator/internal/runtime"
	"kompensator/internal/secrets"
)

// containerTokenPrefix / containerTokenSuffix delimit the placeholder a reload
// command uses to refer to a container by its logical "<stack>/<project>"
// identity, e.g. "{{container:edge/haproxy}}". kompensator generates the real
// container name, so the operator cannot hardcode it; the token is expanded to
// the resolved name(s) just before the command runs.
const (
	containerTokenPrefix = "{{container:"
	containerTokenSuffix = "}}"
)

// materializeFileSecrets writes every file secret the node needs to its host
// path and runs each one's reload hook, but only for secrets whose content
// changed. It runs before the stack deploy loop so a newly written file is in
// place when a project that bind-mounts it (re)starts, and so a "recreate"
// reload can invalidate that project's fingerprint ahead of the loop.
func materializeFileSecrets(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, repoRoot string, env repo.Environment) error {
	for _, secret := range env.Secrets {
		if err := materializeFileSecret(ctx, log, names, opts, repoRoot, secret); err != nil {
			return fmt.Errorf("file secret %q: %w", secret.Name, err)
		}
	}
	return nil
}

// materializeFileSecret handles a single declared file secret: it decrypts the
// blob with the node identity, and — only when the content differs from what is
// already on disk — writes it atomically and triggers the reload hook.
func materializeFileSecret(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, repoRoot string, secret repo.EnvSecret) error {
	if !secret.TargetsNode(names.Node) {
		return nil
	}
	if err := secret.Validate(); err != nil {
		return err
	}
	l := log.With("secret", secret.Name)

	blob, err := os.ReadFile(repo.SecretFileBlob(repoRoot, opts.Env, secret.Name))
	if err != nil {
		if os.IsNotExist(err) {
			l.Warn("file secret declared but no encrypted blob present yet, skipping")
			return nil
		}
		return err
	}
	plaintext, err := secrets.Decrypt(secrets.KeyPath(opts.Home), blob)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	hash := hashBytes(plaintext)

	// Skip when the content is unchanged and the file is already on disk, so an
	// in-sync reconcile never rewrites the file nor re-runs the reload hook.
	unchanged := readSecretHash(opts.Home, opts.Env, secret.Name) == hash
	if unchanged && fileExists(secret.File.Path) && !opts.Force {
		return nil
	}

	if err := writeSecretFile(secret, plaintext); err != nil {
		return err
	}
	l.Info("file secret materialised", "path", secret.File.Path, "bytes", len(plaintext))

	if err := runSecretReload(ctx, l, opts, secret); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return writeSecretHash(opts.Home, opts.Env, secret.Name, hash)
}

// writeSecretFile writes content to the secret's host path atomically (temp
// file + rename) with the declared mode, so a consumer never observes a
// half-written file and the permission is applied before the file is visible.
func writeSecretFile(secret repo.EnvSecret, content []byte) error {
	mode, err := secret.FileMode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(secret.File.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kompensator-secret-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, secret.File.Path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// runSecretReload runs the reload action declared for a changed secret. A
// "recreate" target is realised by invalidating the project's deploy
// fingerprint so the following deploy pass recreates it; a "command" is run
// directly on the node. A secret without a reload block needs no action.
func runSecretReload(ctx context.Context, log *slog.Logger, opts Options, secret repo.EnvSecret) error {
	if secret.Reload == nil {
		return nil
	}
	if secret.Reload.Recreate != "" {
		stack, project, err := splitStackProject(secret.Reload.Recreate)
		if err != nil {
			return err
		}
		log.Info("secret changed, marking project for recreate", "target", secret.Reload.Recreate)
		return invalidateDeployHash(opts.Home, opts.Env, stack, project)
	}
	if len(secret.Reload.Command) == 0 {
		return nil
	}
	return runReloadCommand(ctx, log, opts, secret.Reload.Command)
}

// runReloadCommand runs a reload command, expanding a {{container:...}} token to
// the resolved container name(s) and running the command once per container. If
// the referenced project has no running container yet (e.g. on the very first
// deploy) there is nothing to reload: the freshly written file will be read when
// the project starts, so the command is skipped.
func runReloadCommand(ctx context.Context, log *slog.Logger, opts Options, command []string) error {
	ref, hasToken := containerRef(command)
	if !hasToken {
		log.Info("running secret reload command")
		return runtime.RunCommand(ctx, command)
	}
	stack, project, err := splitStackProject(ref)
	if err != nil {
		return err
	}
	containers, err := runtime.ManagedContainerNames(ctx, "", opts.Env, stack, project)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		log.Info("reload target has no running container yet, skipping reload", "target", ref)
		return nil
	}
	for _, container := range containers {
		log.Info("running secret reload command", "container", container)
		if err := runtime.RunCommand(ctx, expandContainerToken(command, container)); err != nil {
			return err
		}
	}
	return nil
}

// containerRef returns the "<stack>/<project>" reference of the first
// {{container:...}} token in the command, and whether one is present.
func containerRef(command []string) (string, bool) {
	for _, arg := range command {
		if ref, ok := parseContainerToken(arg); ok {
			return ref, true
		}
	}
	return "", false
}

// parseContainerToken extracts the reference inside a {{container:...}} token in
// a single argument.
func parseContainerToken(arg string) (string, bool) {
	start := strings.Index(arg, containerTokenPrefix)
	if start < 0 {
		return "", false
	}
	rest := arg[start+len(containerTokenPrefix):]
	end := strings.Index(rest, containerTokenSuffix)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// expandContainerToken replaces every {{container:...}} token in the command
// with the given container name, returning a new argument slice.
func expandContainerToken(command []string, container string) []string {
	out := make([]string, len(command))
	for i, arg := range command {
		ref, ok := parseContainerToken(arg)
		if !ok {
			out[i] = arg
			continue
		}
		token := containerTokenPrefix + ref + containerTokenSuffix
		out[i] = strings.ReplaceAll(arg, token, container)
	}
	return out
}

// splitStackProject parses a "<stack>/<project>" reference.
func splitStackProject(ref string) (stack, project string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected \"<stack>/<project>\", got %q", ref)
	}
	return parts[0], parts[1], nil
}

// hashBytes returns the hex-encoded SHA-256 of b, used as a secret's content
// fingerprint for change detection.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileExists reports whether a path exists (any stat error is treated as
// absent, which is safe: it only forces a rewrite).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
