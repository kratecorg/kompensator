package admin

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

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
// yet. It diffs against HEAD because 'git add -N' also stages deletions, which
// a plain worktree diff would then no longer show.
func showDiff(ctx context.Context, dest string, paths []string) error {
	args := append([]string{"add", "--intent-to-add", "--"}, paths...)
	if out, err := git(ctx, dest, args...); err != nil {
		return fmt.Errorf("git add -N: %w: %s", err, out)
	}
	defer git(ctx, dest, append([]string{"reset", "--quiet", "--"}, paths...)...)
	out, err := git(ctx, dest, append([]string{"--no-pager", "diff", "HEAD", "--"}, paths...)...)
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

// EnvAddOptions configures scaffolding a new environment.
type EnvAddOptions struct {
	Home      string
	RepoName  string
	Name      string
	Variables []string // "KEY=VALUE" overrides to seed env.yml with
	DryRun    bool
	Logger    *slog.Logger
}

// EnvAdd scaffolds environments/<name>/env.yml. The environment starts out
// placing no stacks; add them with EnvStackAdd.
func EnvAdd(ctx context.Context, opts EnvAddOptions) error {
	log := logger(opts.Logger)
	if err := validName("environment", opts.Name); err != nil {
		return err
	}
	vars, err := parseVars(opts.Variables)
	if err != nil {
		return err
	}
	return withRepo(ctx, opts.Home, opts.RepoName, "environments: add "+opts.Name, opts.DryRun, func(dest string) ([]string, error) {
		rel := path.Join("environments", opts.Name, "env.yml")
		if _, err := os.Stat(repo.EnvironmentFile(dest, opts.Name)); err == nil {
			return nil, fmt.Errorf("environment %q already exists (%s)", opts.Name, rel)
		}
		data, err := render("env.yml.tmpl", map[string]any{"Name": opts.Name, "Variables": vars})
		if err != nil {
			return nil, err
		}
		if err := writeNew(repo.EnvironmentFile(dest, opts.Name), data); err != nil {
			return nil, err
		}
		log.Info("environment scaffolded", "env", opts.Name, "file", rel)
		return []string{rel}, nil
	})
}

// EnvRemove deletes an environment, including its state and secrets. It refuses
// while the environment still places stacks, so nobody drops a live deployment
// target by typing the wrong name.
func EnvRemove(ctx context.Context, home, repoName, env string, dryRun bool, log *slog.Logger) error {
	l := logger(log)
	message := "environments: remove " + env
	return withRepo(ctx, home, repoName, message, dryRun, func(dest string) ([]string, error) {
		e, err := repo.LoadEnvironment(dest, env)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", env, err)
		}
		if names := e.StackNames(); len(names) > 0 {
			return nil, fmt.Errorf("environment %q still places %s; remove the placements first "+
				"('kompensator env stack remove %s <stack>')", env, strings.Join(names, ", "), env)
		}
		if err := os.RemoveAll(repo.EnvironmentDir(dest, env)); err != nil {
			return nil, fmt.Errorf("remove environment %q: %w", env, err)
		}
		l.Info("environment removed", "env", env)
		return []string{path.Join("environments", env)}, nil
	})
}

// EnvStackAddOptions configures placing a stack in an environment.
type EnvStackAddOptions struct {
	Home     string
	RepoName string
	Env      string
	Stack    string
	Nodes    []string // pin the stack to these nodes; empty means every node
	DryRun   bool
	Logger   *slog.Logger
}

// placementEntry is the env.yml shape for a stack pinned to a node subset. An
// unpinned stack is written as a bare name instead.
type placementEntry struct {
	Name  string   `yaml:"name"`
	Nodes []string `yaml:"nodes"`
}

// EnvStackAdd places a stack in an environment by appending it to env.yml's
// stack list, leaving that file's comments intact.
func EnvStackAdd(ctx context.Context, opts EnvStackAddOptions) error {
	log := logger(opts.Logger)
	message := fmt.Sprintf("environments/%s: place stack %s", opts.Env, opts.Stack)
	return withRepo(ctx, opts.Home, opts.RepoName, message, opts.DryRun, func(dest string) ([]string, error) {
		e, err := repo.LoadEnvironment(dest, opts.Env)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", opts.Env, err)
		}
		if _, err := repo.LoadStack(dest, opts.Stack); err != nil {
			return nil, fmt.Errorf("stack %q: %w", opts.Stack, err)
		}
		for _, name := range e.StackNames() {
			if name == opts.Stack {
				return nil, fmt.Errorf("environment %q already places stack %q", opts.Env, opts.Stack)
			}
		}
		if len(opts.Nodes) > 0 {
			inv, err := repo.LoadInventory(dest)
			if err != nil {
				return nil, err
			}
			known := inv.AllNodeNames()
			if len(known) == 0 {
				return nil, fmt.Errorf("the inventory has no nodes yet; add one with 'kompensator node add'")
			}
			for _, node := range opts.Nodes {
				if !inv.Has(node) {
					return nil, fmt.Errorf("node %q is not in the inventory (known: %s)",
						node, strings.Join(known, ", "))
				}
			}
		}

		var entry any = opts.Stack
		if len(opts.Nodes) > 0 {
			entry = placementEntry{Name: opts.Stack, Nodes: opts.Nodes}
		}
		if err := repo.AppendToSequence(repo.EnvironmentFile(dest, opts.Env), "stacks", entry); err != nil {
			return nil, err
		}
		log.Info("stack placed", "env", opts.Env, "stack", opts.Stack, "nodes", strings.Join(opts.Nodes, ","))
		return []string{path.Join("environments", opts.Env, "env.yml")}, nil
	})
}

// EnvStackRemove takes a stack out of an environment. The stack's state and
// secrets files for that environment are left in place, so re-placing it later
// picks up where it left off.
func EnvStackRemove(ctx context.Context, home, repoName, env, stack string, dryRun bool, log *slog.Logger) error {
	l := logger(log)
	message := fmt.Sprintf("environments/%s: unplace stack %s", env, stack)
	return withRepo(ctx, home, repoName, message, dryRun, func(dest string) ([]string, error) {
		removed, err := repo.RemoveFromSequence(repo.EnvironmentFile(dest, env), "stacks", stack)
		if err != nil {
			return nil, err
		}
		if !removed {
			return nil, fmt.Errorf("environment %q does not place stack %q", env, stack)
		}
		l.Info("stack unplaced", "env", env, "stack", stack)
		l.Warn("the stack's containers keep running until the nodes prune them",
			"hint", "kompensator reconcile "+env+" --prune")
		return []string{path.Join("environments", env, "env.yml")}, nil
	})
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

// StackRemove deletes stacks/<name>/ with everything in it. It refuses while an
// environment still places the stack, because a repo in that state no longer
// loads on the nodes.
func StackRemove(ctx context.Context, home, repoName, name string, dryRun bool, log *slog.Logger) error {
	l := logger(log)
	return withRepo(ctx, home, repoName, "stacks: remove "+name, dryRun, func(dest string) ([]string, error) {
		if _, err := os.Stat(repo.StackFile(dest, name)); err != nil {
			return nil, fmt.Errorf("stack %q: %w", name, err)
		}
		envs, err := repo.ListEnvironments(dest)
		if err != nil {
			return nil, err
		}
		var placedIn []string
		for _, env := range envs {
			e, err := repo.LoadEnvironment(dest, env)
			if err != nil {
				return nil, err
			}
			for _, s := range e.StackNames() {
				if s == name {
					placedIn = append(placedIn, env)
				}
			}
		}
		if len(placedIn) > 0 {
			return nil, fmt.Errorf("stack %q is still placed in %s; unplace it first "+
				"('kompensator env stack remove <env> %s')", name, strings.Join(placedIn, ", "), name)
		}
		if err := os.RemoveAll(repo.StackDir(dest, name)); err != nil {
			return nil, fmt.Errorf("remove stack %q: %w", name, err)
		}
		l.Info("stack removed", "stack", name)
		return []string{path.Join("stacks", name)}, nil
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

// ProjectRemove drops a project from its stack and deletes the compose file it
// pointed at. The desired-state entries for the project are left alone, so
// re-adding it later keeps its image pins.
func ProjectRemove(ctx context.Context, home, repoName, stack, name string, dryRun bool, log *slog.Logger) error {
	l := logger(log)
	message := fmt.Sprintf("stacks/%s: remove project %s", stack, name)
	return withRepo(ctx, home, repoName, message, dryRun, func(dest string) ([]string, error) {
		s, err := repo.LoadStack(dest, stack)
		if err != nil {
			return nil, fmt.Errorf("stack %q: %w", stack, err)
		}
		var project *repo.Project
		for i := range s.Projects {
			if s.Projects[i].Name == name {
				project = &s.Projects[i]
			}
		}
		if project == nil {
			return nil, fmt.Errorf("stack %q has no project %q", stack, name)
		}
		removed, err := repo.RemoveFromSequence(repo.StackFile(dest, stack), "projects", name)
		if err != nil {
			return nil, err
		}
		if !removed {
			return nil, fmt.Errorf("stack %q has no project %q", stack, name)
		}
		paths := []string{path.Join("stacks", stack, "stack.yml")}
		// A compose path pointing outside the stack directory is not ours to delete.
		if rel := filepath.Clean(project.Compose); !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "..") {
			if err := os.Remove(repo.ComposeFile(dest, stack, *project)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("remove compose file: %w", err)
			}
			paths = append(paths, path.Join("stacks", stack, filepath.ToSlash(rel)))
		}
		l.Info("project removed", "stack", stack, "project", name)
		l.Warn("the project's containers keep running until the nodes prune them",
			"hint", "kompensator reconcile <env> --prune")
		return paths, nil
	})
}

// StateSetOptions configures pointing one service at an image.
type StateSetOptions struct {
	Home     string
	RepoName string
	Env      string
	Stack    string
	Project  string
	Service  string
	Image    string
	Tag      string
	DryRun   bool
	Logger   *slog.Logger
}

// StateSet writes a service's desired image into environments/<env>/state/
// <stack>.yml and pushes it: this is the call a CI pipeline makes to deploy.
// Only the named service's image and tag change; everything else in the file,
// including its comments, is left as it is.
func StateSet(ctx context.Context, opts StateSetOptions) error {
	log := logger(opts.Logger)
	if opts.Image == "" || opts.Tag == "" {
		return fmt.Errorf("an image and a tag are required")
	}
	message := fmt.Sprintf("%s/%s: %s.%s -> %s:%s",
		opts.Env, opts.Stack, opts.Project, opts.Service, opts.Image, opts.Tag)
	return withRepo(ctx, opts.Home, opts.RepoName, message, opts.DryRun, func(dest string) ([]string, error) {
		e, err := repo.LoadEnvironment(dest, opts.Env)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", opts.Env, err)
		}
		placed := false
		for _, name := range e.StackNames() {
			placed = placed || name == opts.Stack
		}
		if !placed {
			return nil, fmt.Errorf("environment %q does not place stack %q", opts.Env, opts.Stack)
		}
		s, err := repo.LoadStack(dest, opts.Stack)
		if err != nil {
			return nil, fmt.Errorf("stack %q: %w", opts.Stack, err)
		}
		known := false
		for _, p := range s.Projects {
			known = known || p.Name == opts.Project
		}
		if !known {
			return nil, fmt.Errorf("stack %q has no project %q", opts.Stack, opts.Project)
		}

		file := repo.StateFile(dest, opts.Env, opts.Stack)
		rel := path.Join("environments", opts.Env, "state", opts.Stack+".yml")
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			data, err := render("state.yml.tmpl", map[string]any{
				"Env": opts.Env, "Stack": opts.Stack, "Project": opts.Project,
				"Service": opts.Service, "Image": opts.Image, "Tag": opts.Tag,
			})
			if err != nil {
				return nil, err
			}
			if err := writeNew(file, data); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else if err := repo.SetStateImage(file, opts.Project, opts.Service, opts.Image, opts.Tag); err != nil {
			return nil, err
		}
		log.Info("desired image set", "env", opts.Env, "stack", opts.Stack,
			"project", opts.Project, "service", opts.Service, "image", opts.Image+":"+opts.Tag)
		return []string{rel}, nil
	})
}

// variable is one KEY=VALUE override, with the value already rendered as a YAML
// scalar so a port number stays a string.
type variable struct {
	Key   string
	Value string
}

// parseVars turns "KEY=VALUE" arguments into template-ready variables.
func parseVars(raw []string) ([]variable, error) {
	out := make([]variable, 0, len(raw))
	for _, kv := range raw {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid variable %q: expected KEY=VALUE", kv)
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		out = append(out, variable{Key: key, Value: strings.TrimRight(string(encoded), "\n")})
	}
	return out, nil
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
