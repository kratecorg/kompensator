package admin

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/kratecorg/kompensator/internal/gitsync"
	"github.com/kratecorg/kompensator/internal/repo"
)

//go:embed templates
var scaffoldTemplates embed.FS

// withRepo syncs the deployment repo, applies mutate to the checkout and pushes
// the paths mutate reports as one commit. Nothing is pushed unless the changed
// repo still loads, so a scaffolding mistake cannot reach the nodes. With dryRun
// the change is shown as a diff and rolled back instead of committed.
func withRepo(ctx context.Context, home, repoName, message string, dryRun bool, mutate func(dest string) ([]string, error)) error {
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	paths, err := mutate(dest)
	if err != nil {
		return err
	}
	if err := validateRepo(dest); err != nil {
		revertPaths(ctx, dest, paths)
		return fmt.Errorf("refusing to publish a repo that no longer loads: %w", err)
	}
	if dryRun {
		if err := showDiff(ctx, dest, paths); err != nil {
			return err
		}
		revertPaths(ctx, dest, paths)
		return nil
	}
	return gitsync.CommitPush(ctx, dest, r.Branch, message, paths...)
}

// validateRepo loads everything a reconcile would load, so a change that leaves
// the repo unloadable — a stack whose compose file is missing, an environment
// pointing at a stack that is not there — is caught before it is pushed.
func validateRepo(dest string) error {
	if _, err := repo.LoadInventory(dest); err != nil {
		return err
	}
	stacks, err := repo.ListStacks(dest)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(stacks))
	for _, name := range stacks {
		known[name] = true
		s, err := repo.LoadStack(dest, name)
		if err != nil {
			return err
		}
		for _, p := range s.Projects {
			if p.Name == "" {
				return fmt.Errorf("stack %q: a project has no name", name)
			}
			if p.Compose == "" {
				return fmt.Errorf("stack %q project %q: no compose file declared", name, p.Name)
			}
			if _, err := os.Stat(repo.ComposeFile(dest, name, p)); err != nil {
				return fmt.Errorf("stack %q project %q: %w", name, p.Name, err)
			}
		}
	}
	envs, err := repo.ListEnvironments(dest)
	if err != nil {
		return err
	}
	for _, env := range envs {
		e, err := repo.LoadEnvironment(dest, env)
		if err != nil {
			return err
		}
		for _, name := range e.StackNames() {
			if !known[name] {
				return fmt.Errorf("env %q places unknown stack %q", env, name)
			}
		}
	}
	return nil
}

// showDiff prints what the mutation changed, including files git does not track
// yet.
func showDiff(ctx context.Context, dest string, paths []string) error {
	args := append([]string{"add", "--intent-to-add", "--"}, paths...)
	if out, err := git(ctx, dest, args...); err != nil {
		return fmt.Errorf("git add -N: %w: %s", err, out)
	}
	defer git(ctx, dest, append([]string{"reset", "--quiet", "--"}, paths...)...)
	out, err := git(ctx, dest, append([]string{"--no-pager", "diff", "--"}, paths...)...)
	if err != nil {
		return fmt.Errorf("git diff: %w: %s", err, out)
	}
	fmt.Print(out)
	return nil
}

// revertPaths undoes a mutation: tracked files are restored from HEAD, files the
// mutation created are deleted along with any directory it left empty.
func revertPaths(ctx context.Context, dest string, paths []string) {
	for _, p := range paths {
		if _, err := git(ctx, dest, "ls-files", "--error-unmatch", "--", p); err == nil {
			git(ctx, dest, "checkout", "--", p)
			continue
		}
		abs := filepath.Join(dest, p)
		os.Remove(abs)
		for dir := filepath.Dir(abs); strings.HasPrefix(dir, dest+string(filepath.Separator)); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// EnvSummary is one environment as 'env list' reports it.
type EnvSummary struct {
	Name   string
	Stacks []string
}

// EnvList reports the environments the deployment repo defines.
func EnvList(ctx context.Context, home, repoName string) ([]EnvSummary, error) {
	_, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return nil, err
	}
	names, err := repo.ListEnvironments(dest)
	if err != nil {
		return nil, err
	}
	out := make([]EnvSummary, 0, len(names))
	for _, name := range names {
		e, err := repo.LoadEnvironment(dest, name)
		if err != nil {
			return nil, err
		}
		out = append(out, EnvSummary{Name: name, Stacks: e.StackNames()})
	}
	return out, nil
}

// StackSummary is one stack as 'stack list' reports it.
type StackSummary struct {
	Name     string
	Projects []string
	Proxy    string
	Envs     []string // environments that place this stack
}

// StackList reports the stacks the deployment repo defines, and where they are
// placed.
func StackList(ctx context.Context, home, repoName string) ([]StackSummary, error) {
	_, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return nil, err
	}
	names, err := repo.ListStacks(dest)
	if err != nil {
		return nil, err
	}
	envs, err := repo.ListEnvironments(dest)
	if err != nil {
		return nil, err
	}
	placedIn := map[string][]string{}
	for _, env := range envs {
		e, err := repo.LoadEnvironment(dest, env)
		if err != nil {
			return nil, err
		}
		for _, s := range e.StackNames() {
			placedIn[s] = append(placedIn[s], env)
		}
	}
	out := make([]StackSummary, 0, len(names))
	for _, name := range names {
		s, err := repo.LoadStack(dest, name)
		if err != nil {
			return nil, err
		}
		sum := StackSummary{Name: name, Envs: placedIn[name]}
		for _, p := range s.Projects {
			sum.Projects = append(sum.Projects, p.Name)
		}
		if s.Proxy != nil {
			sum.Proxy = s.Proxy.Kind
		}
		out = append(out, sum)
	}
	return out, nil
}

// StackAddOptions configures scaffolding a new stack.
type StackAddOptions struct {
	Home     string
	RepoName string
	Name     string
	Proxy    string // managed proxy kind, e.g. "traefik"; empty for none
	DryRun   bool
	Logger   *slog.Logger
}

// StackAdd scaffolds stacks/<name>/stack.yml. The stack starts out with no
// projects; add them with ProjectAdd.
func StackAdd(ctx context.Context, opts StackAddOptions) error {
	log := logger(opts.Logger)
	if err := validName("stack", opts.Name); err != nil {
		return err
	}
	if opts.Proxy != "" && opts.Proxy != "traefik" {
		return fmt.Errorf("unsupported proxy kind %q (supported: traefik)", opts.Proxy)
	}
	return withRepo(ctx, opts.Home, opts.RepoName, "stacks: add "+opts.Name, opts.DryRun, func(dest string) ([]string, error) {
		rel := filepath.Join("stacks", opts.Name, "stack.yml")
		if _, err := os.Stat(filepath.Join(dest, rel)); err == nil {
			return nil, fmt.Errorf("stack %q already exists (%s)", opts.Name, rel)
		}
		data, err := render("stack.yml.tmpl", map[string]any{"Name": opts.Name, "Proxy": opts.Proxy})
		if err != nil {
			return nil, err
		}
		if err := writeNew(filepath.Join(dest, rel), data); err != nil {
			return nil, err
		}
		log.Info("stack scaffolded", "stack", opts.Name, "file", rel)
		return []string{rel}, nil
	})
}

// ProjectAddOptions configures scaffolding a new project inside a stack.
type ProjectAddOptions struct {
	Home     string
	RepoName string
	Stack    string
	Name     string
	Service  string // compose service name (default: the project name)
	Strategy string // "blue-green" (default) or "recreate"
	Port     int    // container port; also routed through the stack's proxy
	Route    bool   // bind the service to the stack's managed proxy
	DryRun   bool
	Logger   *slog.Logger
}

// projectEntry is the stack.yml shape kompensator writes for a new project. It
// is deliberately narrower than repo.Project so the scaffold emits no empty
// keys a reader would have to ignore.
type projectEntry struct {
	Name     string       `yaml:"name"`
	Compose  string       `yaml:"compose"`
	Strategy string       `yaml:"strategy"`
	Proxy    []routeEntry `yaml:"proxy,omitempty"`
}

type routeEntry struct {
	Router  string `yaml:"router"`
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	Rule    string `yaml:"rule"`
}

// ProjectAdd scaffolds a compose file for a new project and appends the project
// to its stack's stack.yml, leaving that file's comments intact.
func ProjectAdd(ctx context.Context, opts ProjectAddOptions) error {
	log := logger(opts.Logger)
	if err := validName("stack", opts.Stack); err != nil {
		return err
	}
	if err := validName("project", opts.Name); err != nil {
		return err
	}
	service := opts.Service
	if service == "" {
		service = opts.Name
	}
	if err := validName("service", service); err != nil {
		return err
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = repo.StrategyBlueGreen
	}
	if strategy != repo.StrategyBlueGreen && strategy != repo.StrategyRecreate {
		return fmt.Errorf("unknown strategy %q (use %s or %s)", strategy, repo.StrategyBlueGreen, repo.StrategyRecreate)
	}
	port := opts.Port
	if port == 0 {
		port = 8080
	}

	message := fmt.Sprintf("stacks/%s: add project %s", opts.Stack, opts.Name)
	return withRepo(ctx, opts.Home, opts.RepoName, message, opts.DryRun, func(dest string) ([]string, error) {
		s, err := repo.LoadStack(dest, opts.Stack)
		if err != nil {
			return nil, fmt.Errorf("stack %q: %w", opts.Stack, err)
		}
		for _, p := range s.Projects {
			if p.Name == opts.Name {
				return nil, fmt.Errorf("stack %q already has a project %q", opts.Stack, opts.Name)
			}
		}
		if opts.Route && s.Proxy == nil {
			return nil, fmt.Errorf("stack %q has no managed proxy to route through", opts.Stack)
		}

		composeRel := filepath.Join("compose", opts.Name+".yml")
		composePath := filepath.Join(repo.StackDir(dest, opts.Stack), composeRel)
		if _, err := os.Stat(composePath); err == nil {
			return nil, fmt.Errorf("compose file already exists (stacks/%s/%s)", opts.Stack, composeRel)
		}
		data, err := render("compose.yml.tmpl", map[string]any{
			"Stack":     opts.Stack,
			"Project":   opts.Name,
			"Service":   service,
			"Var":       repo.EnvVarName(service),
			"Port":      port,
			"BlueGreen": strategy == repo.StrategyBlueGreen,
		})
		if err != nil {
			return nil, err
		}
		if err := writeNew(composePath, data); err != nil {
			return nil, err
		}

		entry := projectEntry{Name: opts.Name, Compose: filepath.ToSlash(composeRel), Strategy: strategy}
		if opts.Route {
			entry.Proxy = []routeEntry{{Router: opts.Name, Service: service, Port: port, Rule: "PathPrefix(`/`)"}}
		}
		if err := repo.AppendToSequence(repo.StackFile(dest, opts.Stack), "projects", entry); err != nil {
			return nil, err
		}
		log.Info("project scaffolded", "stack", opts.Stack, "project", opts.Name, "compose", composeRel, "strategy", strategy)
		return []string{
			filepath.ToSlash(filepath.Join("stacks", opts.Stack, "stack.yml")),
			filepath.ToSlash(filepath.Join("stacks", opts.Stack, composeRel)),
		}, nil
	})
}

// render executes an embedded scaffold template.
func render(name string, data any) ([]byte, error) {
	t, err := template.ParseFS(scaffoldTemplates, filepath.Join("templates", name))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// writeNew creates a file and the directories leading to it, refusing to
// overwrite an existing one.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// validName rejects names that would not survive being used as a directory, a
// compose project name or an environment variable prefix.
func validName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name required", kind)
	}
	if strings.ContainsAny(name, `/\ .`) || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid %s name %q: use letters, digits, '-' and '_'", kind, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("invalid %s name %q: use letters, digits, '-' and '_'", kind, name)
		}
	}
	return nil
}
