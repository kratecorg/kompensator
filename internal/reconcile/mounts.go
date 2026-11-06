package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/kratecorg/kompensator/internal/repo"
	"github.com/kratecorg/kompensator/internal/runtime"
)

// repoBindMounts returns the compose file's bind-mount sources that live in the
// repo checkout. Host paths outside it (/etc/localtime, certificates, sockets)
// are not versioned by the repo and are deliberately not tracked.
func repoBindMounts(ctx context.Context, repoRoot, composeFile string, extraEnv []string) ([]string, error) {
	sources, err := runtime.ComposeBindSources(ctx, composeFile, append(os.Environ(), extraEnv...))
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	var inRepo []string
	for _, source := range sources {
		if isWithin(root, source) {
			inRepo = append(inRepo, source)
		}
	}
	return inRepo, nil
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// hashMounts fingerprints the content of the given mount sources, recursing
// into directories. It returns "" when there are no mounts so a project without
// repo mounts keeps the config hash it had before mounts were tracked.
func hashMounts(repoRoot string, mounts []string) (string, error) {
	if len(mounts) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, mount := range mounts {
		err := filepath.WalkDir(mount, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintf(h, "%s\x00missing\x00", relativeTo(repoRoot, path))
					return nil
				}
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00", relativeTo(repoRoot, path))
			h.Write(content)
			h.Write([]byte{0})
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("hash mount %s: %w", mount, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// relativeTo keeps the fingerprint independent of where the repo is checked out.
func relativeTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// reloadOnMountChange signals the project's containers when its bind-mounted
// repo files changed since the last reload. A deploy in this same pass already
// started containers on the new content, so it only records the fingerprint.
func reloadOnMountChange(ctx context.Context, log *slog.Logger, opts Options, stack string, p repo.Project, mounts []string, mountHash string, deployed bool, res *Result) error {
	if !deployed && readMountHash(opts.Home, opts.Env, stack, p.Name) == mountHash {
		return nil
	}
	if !deployed {
		warnFileMounts(log, mounts)
		containers, err := runtime.SignalManagedContainers(ctx, "", opts.Env, stack, p.Name, p.Reload.Service, p.Reload.Signal)
		if err != nil {
			return fmt.Errorf("reload: %w", err)
		}
		log.Info("mounted config changed, reloaded", "signal", p.Reload.Signal, "containers", containers)
		res.Reloaded++
	}
	return writeMountHash(opts.Home, opts.Env, stack, p.Name, mountHash)
}

// warnFileMounts flags single-file mounts: git replaces a changed file with a
// new inode, so a running container keeps reading the old one and a reload
// cannot pick up the change. Mounting the parent directory avoids that.
func warnFileMounts(log *slog.Logger, mounts []string) {
	for _, mount := range mounts {
		info, err := os.Stat(mount)
		if err != nil || info.IsDir() {
			continue
		}
		log.Warn("file bind mount cannot be reloaded live, mount its directory instead", "source", mount)
	}
}

func readMountHash(home, env, stack, project string) string {
	d, err := loadStatusDoc(home, env)
	if err != nil {
		return ""
	}
	if p := d.Projects[deployKey(stack, project)]; p != nil {
		return p.MountHash
	}
	return ""
}

func writeMountHash(home, env, stack, project, hash string) error {
	d, err := loadStatusDoc(home, env)
	if err != nil {
		return err
	}
	if d.Env == "" {
		d.Env = env
	}
	d.project(deployKey(stack, project)).MountHash = hash
	return d.save(home, env)
}
