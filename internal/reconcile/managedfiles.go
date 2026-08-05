package reconcile

import (
	"context"
	"fmt"
	"log/slog"

	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// materializeManagedFiles writes every managed file the node needs to its host
// path and runs each one's reload hook, but only for files whose content
// changed. Like file secrets it runs before the stack deploy loop, so a newly
// written file is in place when a project that bind-mounts it starts.
func materializeManagedFiles(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, env repo.Environment) error {
	for _, file := range env.Files {
		if err := materializeManagedFile(ctx, log, names, opts, env, file); err != nil {
			return fmt.Errorf("managed file %q: %w", file.Name, err)
		}
	}
	return nil
}

// materializeManagedFile resolves one declared file's variable for this node
// and — only when the value differs from what is already on disk — writes it
// atomically and triggers the reload hook.
func materializeManagedFile(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, env repo.Environment, file repo.ManagedFile) error {
	if !file.TargetsNode(names.Node) {
		return nil
	}
	if err := file.Validate(); err != nil {
		return err
	}
	value, err := file.ResolveValue(env, names.Node)
	if err != nil {
		return err
	}

	// A trailing newline makes the file readable by line-oriented consumers
	// (shell $(cat), read) without changing the value they see.
	content := []byte(value + "\n")
	hash := hashBytes(content)

	unchanged := readFileHash(opts.Home, opts.Env, file.Name) == hash
	if unchanged && fileExists(file.Path) && !opts.Force {
		return nil
	}

	mode, err := file.FileMode()
	if err != nil {
		return err
	}
	if err := writeNodeFile(file.Path, mode, content); err != nil {
		return err
	}
	l := log.With("file", file.Name)
	l.Info("managed file materialised", "path", file.Path, "variable", file.Variable, "value", value)

	if err := runReload(ctx, l, opts, file.Reload); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return writeFileHash(opts.Home, opts.Env, file.Name, hash)
}
