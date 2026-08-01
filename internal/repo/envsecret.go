package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// defaultSecretFileMode is the permission a materialised secret file gets when
// the declaration does not pin one. It is deliberately restrictive: a secret is
// readable only by its owner unless the operator explicitly widens it (e.g. to
// 0640 so a container's group can read a shared certificate).
const defaultSecretFileMode = 0o600

// EnvSecret declares one environment-scoped file secret. Unlike the flat
// KEY: value stack secrets (which become container environment variables), a
// file secret is an opaque age-encrypted blob that kompensator writes verbatim
// to a host path on the nodes that need it, then optionally triggers a reload.
//
// The encrypted blob lives at environments/<env>/secrets/files/<name>.age and
// is authored with `kompensator secrets set-file`. It is decrypted on the node
// during reconcile and only rewritten when its content actually changed, so a
// consumer (e.g. haproxy) can be reloaded rather than restarted.
type EnvSecret struct {
	Name string `yaml:"name"`
	// Nodes optionally pins the secret to a subset of the environment's nodes.
	// An empty list means every node that runs the environment receives it. It
	// accepts a scalar or a sequence, like a stack placement pin.
	Nodes NodeList `yaml:"nodes,omitempty"`
	// File describes where and how the decrypted blob is written on the node.
	File *SecretFile `yaml:"file,omitempty"`
	// Reload is the single action to run after the file's content changed. It is
	// never run when the content is unchanged, so an in-sync reconcile is inert.
	Reload *SecretReload `yaml:"reload,omitempty"`
}

// SecretFile describes the on-node destination of a materialised secret.
type SecretFile struct {
	Path string `yaml:"path"`
	// Mode is the file's octal permission string (e.g. "0640"). Empty means the
	// restrictive default (0600).
	Mode string `yaml:"mode,omitempty"`
}

// SecretReload is the reaction to a changed secret. Exactly one of Command or
// Recreate may be set. Neither is required: a file bind-mounted into a project
// that reads it only at start needs no reload beyond the project's own deploy.
//
// Command runs an arbitrary command on the node — typically a live reload that
// does not drop connections, e.g. `docker exec <haproxy> kill -HUP 1`. The
// literal container name is unknown to the operator (kompensator generates it),
// so the token {{container:<stack>/<project>}} in any command argument is
// expanded to the resolved container name(s); the command runs once per match.
//
// Recreate names a "<stack>/<project>" that kompensator recreates on change, by
// invalidating that project's deploy fingerprint so the regular reconcile pass
// redeploys it with --force-recreate. Use it for consumers that cannot reload
// their config live.
type SecretReload struct {
	Command  []string `yaml:"command,omitempty"`
	Recreate string   `yaml:"recreate,omitempty"`
}

// Validate checks a secret declaration is well-formed. It is called both when
// authoring a blob (to reject orphan writes) and can be used at load time.
func (s EnvSecret) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("secret has no name")
	}
	if s.File == nil {
		return fmt.Errorf("secret %q has no file: block", s.Name)
	}
	if s.File.Path == "" {
		return fmt.Errorf("secret %q has an empty file.path", s.Name)
	}
	if _, err := s.FileMode(); err != nil {
		return fmt.Errorf("secret %q: %w", s.Name, err)
	}
	if s.Reload != nil {
		if len(s.Reload.Command) > 0 && s.Reload.Recreate != "" {
			return fmt.Errorf("secret %q: reload sets both command and recreate; pick one", s.Name)
		}
	}
	return nil
}

// FileMode parses the declared octal mode, or the restrictive default when the
// declaration omits it.
func (s EnvSecret) FileMode() (os.FileMode, error) {
	if s.File == nil || s.File.Mode == "" {
		return defaultSecretFileMode, nil
	}
	parsed, err := strconv.ParseUint(s.File.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid file.mode %q (want an octal string like \"0640\"): %w", s.File.Mode, err)
	}
	return os.FileMode(parsed), nil
}

// TargetsNode reports whether the secret should be materialised on the node.
// An unpinned secret targets every node of the environment; a pinned one only
// the nodes in its list. Callers first ensure the node runs the environment.
func (s EnvSecret) TargetsNode(node string) bool {
	return len(s.Nodes) == 0 || s.Nodes.has(node)
}

// Secret returns the environment's declaration for a secret name.
func (e Environment) Secret(name string) (EnvSecret, bool) {
	for _, s := range e.Secrets {
		if s.Name == name {
			return s, true
		}
	}
	return EnvSecret{}, false
}

// ParticipatingNodes returns the subset of allNodes that run at least one stack
// of the environment. It is the recipient set for an unpinned file secret.
func (e Environment) ParticipatingNodes(allNodes []string) []string {
	var out []string
	for _, node := range allNodes {
		if e.RunsOnNode(node) {
			out = append(out, node)
		}
	}
	return out
}

// SecretsFilesDir is the directory holding an environment's encrypted file
// secrets: environments/<env>/secrets/files.
func SecretsFilesDir(repoRoot, env string) string {
	return filepath.Join(repoRoot, "environments", env, "secrets", "files")
}

// SecretFileBlob returns the absolute path to a file secret's age-encrypted
// blob: environments/<env>/secrets/files/<name>.age.
func SecretFileBlob(repoRoot, env, name string) string {
	return filepath.Join(SecretsFilesDir(repoRoot, env), name+".age")
}
