package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"kompensator/internal/config"
	"kompensator/internal/gitsync"
	"kompensator/internal/repo"
	"kompensator/internal/runtime"
	"kompensator/internal/secrets"
)

// healthTimeout bounds how long a deploy waits for new containers to become
// healthy before giving up and reporting the project as failed.
const healthTimeout = 5 * time.Minute

// Options controls a reconcile run.
type Options struct {
	Home    string
	Env     string
	Force   bool // redeploy even when the desired images are already running
	JSONLog bool // pass -json through to node agents (controller mode)
	Logger  *slog.Logger
}

// Result summarises a reconcile run, counted per project.
type Result struct {
	Deployed int
	InSync   int
	Skipped  int
	Failed   int
}

// Run performs a reconcile. On a node agent (config has node.name) it does a
// single local pass: sync each deployment repo, resolve the stacks/projects
// placed on this node for the environment, and deploy any that have drifted. On
// a controller (config has no node.name) it instead triggers each participating
// node's agent — locally by re-executing itself with the node's home, or
// remotely over ssh.
func Run(ctx context.Context, opts Options) (Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	cfg, err := config.Load(opts.Home)
	if err != nil {
		return Result{}, err
	}

	if cfg.IsController() {
		return runController(ctx, log, opts, cfg)
	}
	return runNode(ctx, log, opts, cfg)
}

// runNode is the node-local reconcile pass (cron and manual entrypoint).
func runNode(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) (Result, error) {
	unlock, held, err := lock(opts.Home)
	if err != nil {
		return Result{}, err
	}
	if !held {
		log.Info("another reconcile is in progress, skipping")
		return Result{}, nil
	}
	defer unlock()

	log = log.With("node", cfg.Node.Name, "env", opts.Env)
	log.Info("reconcile started")

	var total Result
	for _, r := range cfg.Repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return total, fmt.Errorf("sync repo %q: %w", r.Name, err)
		}
		log.Info("repo synced", "repo", r.Name, "commit", commit)

		names := runtime.Names{
			Repo: r.Name, Node: cfg.Node.Name,
			IncludeRepo: cfg.Naming.UseRepo(), IncludeNode: cfg.Naming.UseNode(),
		}
		res, err := reconcileRepo(ctx, log, names, opts, dest)
		total.add(res)
		if err != nil {
			return total, fmt.Errorf("reconcile repo %q: %w", r.Name, err)
		}
	}

	log.Info("reconcile finished",
		"deployed", total.Deployed,
		"in_sync", total.InSync,
		"skipped", total.Skipped,
		"failed", total.Failed,
	)
	if total.Failed > 0 {
		return total, fmt.Errorf("%d project(s) failed to reconcile", total.Failed)
	}
	return total, nil
}

// runController triggers reconcile on every node that participates in the
// environment. Nodes are processed one at a time so a Blue/Green rollout never
// runs on two nodes simultaneously. A failure on one node is recorded and the
// rest still run.
func runController(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) (Result, error) {
	unlock, held, err := lock(opts.Home)
	if err != nil {
		return Result{}, err
	}
	if !held {
		log.Info("another controller run is in progress, skipping")
		return Result{}, nil
	}
	defer unlock()

	clog := log.With("controller", true, "env", opts.Env)
	clog.Info("controller reconcile started")

	targets, err := reconcileTargets(ctx, clog, opts, cfg)
	if err != nil {
		return Result{}, err
	}
	if len(targets) == 0 {
		clog.Info("no nodes participate in env")
		return Result{}, nil
	}

	var failed int
	for _, t := range targets {
		nlog := clog.With("node", t.name)
		nlog.Info("triggering node reconcile", "location", t.loc.Path, "remote", !t.loc.Local)
		if err := triggerReconcile(ctx, t.loc, opts); err != nil {
			nlog.Error("node reconcile failed", "error", err)
			failed++
			continue
		}
		nlog.Info("node reconcile ok")
	}

	clog.Info("controller reconcile finished", "nodes", len(targets), "failed", failed)
	if failed > 0 {
		return Result{Failed: failed}, fmt.Errorf("%d of %d node(s) failed to reconcile", failed, len(targets))
	}
	return Result{}, nil
}

// reconcileTarget is one node the controller will trigger.
type reconcileTarget struct {
	name string
	loc  repo.Location
}

// reconcileTargets syncs the controller's repos and returns the nodes that
// participate in the environment, deduplicated by name. Every such node must
// declare a location so the controller can reach it.
func reconcileTargets(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) ([]reconcileTarget, error) {
	seen := map[string]bool{}
	var targets []reconcileTarget
	for _, r := range cfg.Repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return nil, fmt.Errorf("sync repo %q: %w", r.Name, err)
		}
		log.Info("repo synced", "repo", r.Name, "commit", commit)

		inv, err := repo.LoadInventory(dest)
		if err != nil {
			return nil, fmt.Errorf("repo %q: %w", r.Name, err)
		}
		for _, n := range inv.Nodes {
			if !inv.InEnv(n.Name, opts.Env) {
				continue // node not in this environment
			}
			if seen[n.Name] {
				continue
			}
			if n.Location == "" {
				return nil, fmt.Errorf("node %q has no location; controller cannot trigger reconcile", n.Name)
			}
			loc, err := repo.ParseLocation(n.Location)
			if err != nil {
				return nil, fmt.Errorf("node %q: %w", n.Name, err)
			}
			seen[n.Name] = true
			targets = append(targets, reconcileTarget{name: n.Name, loc: loc})
		}
	}
	return targets, nil
}

// triggerReconcile runs the node agent for one node. A local node is reached by
// re-executing this binary with the node's home; a remote node over ssh, where
// the agent binary lives next to its config inside the node home
// (<home>/kompensator). The agent's own logs stream to the controller's stderr.
func triggerReconcile(ctx context.Context, loc repo.Location, opts Options) error {
	global := []string{"-home", loc.Path}
	if opts.JSONLog {
		global = append(global, "-json")
	}
	sub := []string{"reconcile"}
	if opts.Force {
		sub = append(sub, "--force")
	}
	sub = append(sub, opts.Env)

	var cmd *exec.Cmd
	if loc.Local {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve own binary: %w", err)
		}
		cmd = exec.CommandContext(ctx, self, append(global, sub...)...)
	} else {
		agentBin := loc.Path + "/kompensator"
		sshArgs := []string{"-o", "BatchMode=yes"}
		if loc.Port != "" {
			sshArgs = append(sshArgs, "-p", loc.Port)
		}
		target := loc.Host
		if loc.User != "" {
			target = loc.User + "@" + loc.Host
		}
		remote := append([]string{agentBin}, append(global, sub...)...)
		sshArgs = append(sshArgs, target, strings.Join(remote, " "))
		cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// reconcileRepo reconciles every stack placed in the environment for this repo.
func reconcileRepo(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, repoRoot string) (Result, error) {
	var res Result

	inv, err := repo.LoadInventory(repoRoot)
	if err != nil {
		return res, err
	}
	if !inv.InEnv(names.Node, opts.Env) {
		log.Info("node not in this repo's env, skipping repo", "repo", repoRoot)
		return res, nil
	}

	env, err := repo.LoadEnvironment(repoRoot, opts.Env)
	if err != nil {
		return res, err
	}
	if len(env.Stacks) == 0 {
		log.Info("env hosts no stacks")
		return res, nil
	}

	for _, stackName := range env.Stacks {
		stack, err := repo.LoadStack(repoRoot, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q: %w", stackName, err)
		}
		state, err := repo.LoadStackState(repoRoot, opts.Env, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q: %w", stackName, err)
		}
		vars := repo.MergeVariables(stack.Variables, env.Variables)
		secretVars, err := loadSecretVars(opts.Home, repoRoot, opts.Env, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q secrets: %w", stackName, err)
		}
		for k, v := range secretVars {
			vars[k] = v
		}
		for _, p := range stack.Projects {
			if err := reconcileProject(ctx, log, names, opts, repoRoot, stackName, p, vars, state[p.Name], &res); err != nil {
				log.Error("project reconcile failed", "stack", stackName, "project", p.Name, "error", err)
				res.Failed++
			}
		}
	}
	return res, nil
}

// loadSecretVars decrypts the stack's secrets file for the environment, if one
// exists, using the node's own age identity. It returns nil when no secrets
// file is present. A present file with no usable identity (e.g. the node was
// provisioned before secrets support, or is not a recipient) is an error so a
// missing secret never silently degrades a deploy.
func loadSecretVars(home, repoRoot, env, stack string) (map[string]string, error) {
	path := repo.SecretsFile(repoRoot, env, stack)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return secrets.DecryptMap(secrets.KeyPath(home), data)
}

// reconcileProject brings one compose project to its desired state, using its
// Blue/Green or recreate strategy.
func reconcileProject(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, repoRoot, stack string, p repo.Project, vars map[string]string, desired map[string]repo.ServiceImage, res *Result) error {
	plog := log.With("stack", stack, "project", p.Name, "strategy", p.Strategy)

	if len(desired) == 0 {
		plog.Warn("no desired state for project, skipping")
		res.Skipped++
		return nil
	}
	for svc, si := range desired {
		if si.Image == "" || si.Tag == "" {
			plog.Warn("service has no image/tag, skipping project", "service", svc)
			res.Skipped++
			return nil
		}
	}

	extraEnv, desiredRefs := buildEnv(opts.Env, vars, desired)
	composeFile := repo.ComposeFile(repoRoot, stack, p)

	if p.BlueGreen() {
		return blueGreenProject(ctx, plog, names, opts, composeFile, stack, p.Name, extraEnv, desiredRefs, res)
	}
	return recreateProject(ctx, plog, names, opts, composeFile, stack, p.Name, extraEnv, desiredRefs, res)
}

// recreateProject deploys a project in place (no color). Used for projects that
// cannot run two colors at once, e.g. a database.
func recreateProject(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, composeFile, stack, project string, extraEnv []string, desiredRefs map[string]string, res *Result) error {
	proj := names.Project(opts.Env, stack, project, "")

	running, err := runtime.ProjectImages(ctx, "", proj)
	if err != nil {
		return err
	}
	if imagesMatch(running, desiredRefs) && !opts.Force {
		log.Info("in sync", "images", describeRefs(desiredRefs))
		res.InSync++
		return nil
	}

	reason := "drift detected, recreating project"
	if opts.Force {
		reason = "force redeploy (recreate)"
	}
	log.Info(reason, "running", describeImages(running), "desired", describeRefs(desiredRefs))

	if err := runtime.Deploy(ctx, composeFile, proj, names.Node, extraEnv); err != nil {
		return err
	}
	if err := runtime.WaitHealthy(ctx, proj, healthTimeout); err != nil {
		return fmt.Errorf("project not healthy: %w", err)
	}
	got, err := runtime.ProjectImages(ctx, "", proj)
	if err != nil {
		return err
	}
	if !imagesMatch(got, desiredRefs) {
		return fmt.Errorf("after deploy, running images %s != desired %s", describeImages(got), describeRefs(desiredRefs))
	}

	log.Info("deployed", "images", describeRefs(desiredRefs))
	res.Deployed++
	return nil
}

// blueGreenProject deploys a project into the idle color, waits for it to
// become healthy, verifies its images, then stops the previously active color.
func blueGreenProject(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, composeFile, stack, project string, extraEnv []string, desiredRefs map[string]string, res *Result) error {
	running, err := runningByColor(ctx, "", names, opts.Env, stack, project)
	if err != nil {
		return err
	}

	// A color that fully serves the desired images means we are in sync; stop
	// any stale other color left behind by a prior switch. --force overrides.
	if active := colorServing(running, desiredRefs); active != "" && !opts.Force {
		log.Info("in sync", "images", describeRefs(desiredRefs), "color", active)
		if err := stopOtherColors(ctx, log, names, opts.Env, stack, project, active, running); err != nil {
			return err
		}
		res.InSync++
		return nil
	}

	target := runtime.OtherColor(currentColor(running))
	targetProject := names.Project(opts.Env, stack, project, target)

	reason := "drift detected, deploying to idle color"
	if opts.Force {
		reason = "force redeploy to idle color"
	}
	log.Info(reason, "running", describeColors(running), "desired", describeRefs(desiredRefs), "target_color", target)

	if err := runtime.Deploy(ctx, composeFile, targetProject, names.Node, extraEnv); err != nil {
		return err
	}
	log.Info("waiting for new color to become healthy", "color", target, "project", targetProject)
	if err := runtime.WaitHealthy(ctx, targetProject, healthTimeout); err != nil {
		return fmt.Errorf("new color %q not healthy: %w", target, err)
	}

	got, err := runtime.ProjectImages(ctx, "", targetProject)
	if err != nil {
		return err
	}
	if !imagesMatch(got, desiredRefs) {
		return fmt.Errorf("after deploy, running images %s != desired %s", describeImages(got), describeRefs(desiredRefs))
	}
	log.Info("new color healthy", "color", target, "images", describeRefs(desiredRefs))

	if err := stopOtherColors(ctx, log, names, opts.Env, stack, project, target, running); err != nil {
		return err
	}

	log.Info("deployed", "images", describeRefs(desiredRefs), "color", target)
	res.Deployed++
	return nil
}

// buildEnv assembles the compose env vars for a deploy: the stack/env variables
// (lowest precedence), the built-in ENV_NAME, and the per-service
// <SERVICE>_IMAGE / <SERVICE>_TAG values (highest precedence, so user variables
// can never break image injection). NODE_NAME is added separately by Deploy. It
// also returns the desired image ref per service.
func buildEnv(envName string, vars map[string]string, desired map[string]repo.ServiceImage) (extraEnv []string, refs map[string]string) {
	merged := make(map[string]string, len(vars)+len(desired)*2+1)
	for k, v := range vars {
		merged[k] = v
	}
	merged["ENV_NAME"] = envName

	refs = make(map[string]string, len(desired))
	for svc, si := range desired {
		v := envVarName(svc)
		merged[v+"_IMAGE"] = si.Image
		merged[v+"_TAG"] = si.Tag
		refs[svc] = si.Ref()
	}

	extraEnv = make([]string, 0, len(merged))
	for k, v := range merged {
		extraEnv = append(extraEnv, k+"="+v)
	}
	sort.Strings(extraEnv)
	return extraEnv, refs
}

// envVarName converts a service name to an uppercase env-var-safe prefix, e.g.
// "frontend" -> "FRONTEND", "next-cloud" -> "NEXT_CLOUD".
func envVarName(service string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, service)
}

// runningByColor returns, for each Blue/Green color that has running
// containers, the service->image map it serves.
func runningByColor(ctx context.Context, host string, names runtime.Names, env, stack, project string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, color := range runtime.Colors {
		proj := names.Project(env, stack, project, color)
		imgs, err := runtime.ProjectImages(ctx, host, proj)
		if err != nil {
			return nil, err
		}
		if len(imgs) > 0 {
			out[color] = imgs
		}
	}
	return out, nil
}

// colorServing returns the color whose running images fully match the desired
// refs, or "" if none does.
func colorServing(running map[string]map[string]string, desiredRefs map[string]string) string {
	for _, color := range runtime.Colors {
		if imagesMatch(running[color], desiredRefs) {
			return color
		}
	}
	return ""
}

// currentColor returns the color to treat as currently active, or "" when
// nothing is running (so the first deploy lands in blue).
func currentColor(running map[string]map[string]string) string {
	for _, color := range runtime.Colors {
		if len(running[color]) > 0 {
			return color
		}
	}
	return ""
}

// stopOtherColors tears down every running color except keep.
func stopOtherColors(ctx context.Context, log *slog.Logger, names runtime.Names, env, stack, project, keep string, running map[string]map[string]string) error {
	for _, color := range runtime.Colors {
		if color == keep || len(running[color]) == 0 {
			continue
		}
		proj := names.Project(env, stack, project, color)
		log.Info("stopping old color", "color", color, "project", proj)
		if err := runtime.Stop(ctx, proj); err != nil {
			return err
		}
	}
	return nil
}

// imagesMatch reports whether running serves exactly the desired refs for every
// desired service. running must be non-empty.
func imagesMatch(running map[string]string, desiredRefs map[string]string) bool {
	if len(running) == 0 {
		return false
	}
	for svc, ref := range desiredRefs {
		if running[svc] != ref {
			return false
		}
	}
	return true
}

func describeRefs(refs map[string]string) string {
	parts := make([]string, 0, len(refs))
	for svc, ref := range refs {
		parts = append(parts, svc+"="+ref)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func describeImages(imgs map[string]string) string {
	if len(imgs) == 0 {
		return "<none>"
	}
	return describeRefs(imgs)
}

func describeColors(running map[string]map[string]string) string {
	if len(running) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(running))
	for _, color := range runtime.Colors {
		if imgs, ok := running[color]; ok {
			parts = append(parts, color+"["+describeRefs(imgs)+"]")
		}
	}
	return strings.Join(parts, ",")
}

func (r *Result) add(o Result) {
	r.Deployed += o.Deployed
	r.InSync += o.InSync
	r.Skipped += o.Skipped
	r.Failed += o.Failed
}

// ServiceStatus is the desired vs. running state of one service of one project,
// on one node. One ServiceStatus is emitted per running container (replica); a
// service with nothing running yields a single row with empty Container/Running.
type ServiceStatus struct {
	Node      string
	Repo      string
	Env       string
	Stack     string
	Project   string
	Service   string
	Desired   string // "image:tag", or "" if absent from the stack state
	Color     string // Blue/Green slot, or "" (recreate / not running)
	Container string // short container name, e.g. "frontend-1", or ""
	Running   string // "image:tag" of this container, or "" if not running
	Health    string // concise health token
}

// State summarises the row's status as a short word.
func (s ServiceStatus) State() string {
	switch {
	case s.Desired == "":
		return "no-target"
	case s.Running == "":
		return "missing"
	case s.Running == s.Desired:
		return "in-sync"
	default:
		return "drift"
	}
}

// Status collects the desired vs. running state for every service, without
// changing anything. It refreshes the repo(s) first when no reconcile is
// currently running; if one is, it reports the last-synced state.
//
// When the inventory declares node locations, Status acts as a controller and
// aggregates every node (querying each node's docker daemon via its location).
// Otherwise it reports only this node, against the local daemon. When opts.Env
// is empty, every environment a node participates in is reported.
func Status(ctx context.Context, opts Options) ([]ServiceStatus, error) {
	cfg, err := config.Load(opts.Home)
	if err != nil {
		return nil, err
	}

	unlock, held, err := lock(opts.Home)
	if err != nil {
		return nil, err
	}
	if held {
		defer unlock()
	}
	canPull := held

	var out []ServiceStatus
	for _, r := range cfg.Repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		if canPull {
			if _, err := gitsync.Sync(ctx, r.URL, r.Branch, dest); err != nil {
				return out, fmt.Errorf("sync repo %q: %w", r.Name, err)
			}
		}

		inv, err := repo.LoadInventory(dest)
		if err != nil {
			return out, fmt.Errorf("repo %q: %w", r.Name, err)
		}

		targets, err := statusTargets(inv, cfg.Node.Name)
		if err != nil {
			return out, fmt.Errorf("repo %q: %w", r.Name, err)
		}

		for _, t := range targets {
			names := runtime.Names{
				Repo: r.Name, Node: t.node,
				IncludeRepo: cfg.Naming.UseRepo(), IncludeNode: cfg.Naming.UseNode(),
			}
			for _, env := range statusEnvs(inv, t.node, opts.Env) {
				if !inv.InEnv(t.node, env) {
					continue
				}
				rows, err := statusEnv(ctx, dest, names, t, env)
				if err != nil {
					return out, err
				}
				out = append(out, rows...)
			}
		}
	}
	return out, nil
}

// statusEnv reports every service of every stack placed in one environment on
// one node.
func statusEnv(ctx context.Context, repoRoot string, names runtime.Names, t statusTarget, env string) ([]ServiceStatus, error) {
	e, err := repo.LoadEnvironment(repoRoot, env)
	if err != nil {
		return nil, fmt.Errorf("repo %q env %q: %w", names.Repo, env, err)
	}

	var out []ServiceStatus
	for _, stackName := range e.Stacks {
		stack, err := repo.LoadStack(repoRoot, stackName)
		if err != nil {
			return nil, fmt.Errorf("repo %q stack %q: %w", names.Repo, stackName, err)
		}
		state, err := repo.LoadStackState(repoRoot, env, stackName)
		if err != nil {
			return nil, fmt.Errorf("repo %q stack %q: %w", names.Repo, stackName, err)
		}
		for _, p := range stack.Projects {
			rows, err := statusProject(ctx, t, names, env, stackName, p, state[p.Name])
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
	}
	return out, nil
}

// statusProject reports every service of one project: the desired image, and a
// row per running container (or a single missing row).
func statusProject(ctx context.Context, t statusTarget, names runtime.Names, env, stack string, p repo.Project, desired map[string]repo.ServiceImage) ([]ServiceStatus, error) {
	containers, err := projectContainers(ctx, t.dockerHost, names, env, stack, p)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", t.node, err)
	}

	bySvc := map[string][]runtime.Container{}
	for _, c := range containers {
		bySvc[c.Service] = append(bySvc[c.Service], c)
	}

	// Report the union of declared (desired) and actually running services.
	serviceNames := map[string]bool{}
	for svc := range desired {
		serviceNames[svc] = true
	}
	for svc := range bySvc {
		serviceNames[svc] = true
	}
	sorted := make([]string, 0, len(serviceNames))
	for svc := range serviceNames {
		sorted = append(sorted, svc)
	}
	sort.Strings(sorted)

	var out []ServiceStatus
	for _, svc := range sorted {
		desiredRef := ""
		if si, ok := desired[svc]; ok && si.Image != "" && si.Tag != "" {
			desiredRef = si.Ref()
		}
		base := ServiceStatus{
			Node: t.node, Repo: names.Repo, Env: env,
			Stack: stack, Project: p.Name, Service: svc, Desired: desiredRef,
		}
		cs := bySvc[svc]
		if len(cs) == 0 {
			out = append(out, base)
			continue
		}
		for _, c := range cs {
			st := base
			st.Color = c.Color
			st.Container = c.Name
			st.Running = c.Image
			st.Health = c.Health
			out = append(out, st)
		}
	}
	return out, nil
}

// projectContainers lists a project's containers across the relevant compose
// projects: both colors for Blue/Green, the single colorless project otherwise.
func projectContainers(ctx context.Context, host string, names runtime.Names, env, stack string, p repo.Project) ([]runtime.Container, error) {
	if !p.BlueGreen() {
		proj := names.Project(env, stack, p.Name, "")
		return runtime.ProjectContainers(ctx, host, proj)
	}
	var all []runtime.Container
	for _, color := range runtime.Colors {
		proj := names.Project(env, stack, p.Name, color)
		cs, err := runtime.ProjectContainers(ctx, host, proj)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			c.Color = color
			all = append(all, c)
		}
	}
	return all, nil
}

// statusTarget is one node to report on, with the docker endpoint to query.
type statusTarget struct {
	node       string
	dockerHost string // "" = local daemon
}

// statusTargets decides which nodes to report. When the inventory declares node
// locations we are a controller: report every node, querying each via its
// location's docker endpoint (the self node uses the local daemon). Without
// locations we report only the configured node against the local daemon.
func statusTargets(inv repo.Inventory, selfNode string) ([]statusTarget, error) {
	hasLocations := false
	for _, n := range inv.Nodes {
		if n.Location != "" {
			hasLocations = true
			break
		}
	}

	if !hasLocations {
		if selfNode == "" {
			return nil, fmt.Errorf("no node locations in inventory and no node.name in config: nothing to report")
		}
		return []statusTarget{{node: selfNode}}, nil
	}

	targets := make([]statusTarget, 0, len(inv.Nodes))
	for _, n := range inv.Nodes {
		if n.Location == "" {
			return nil, fmt.Errorf("node %q has no location (all or no nodes must declare one)", n.Name)
		}
		loc, err := repo.ParseLocation(n.Location)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Name, err)
		}
		host := loc.DockerHost()
		if n.Name == selfNode {
			host = "" // we are this node: query the local daemon directly
		}
		targets = append(targets, statusTarget{node: n.Name, dockerHost: host})
	}
	return targets, nil
}

// statusEnvs returns the environments to report for a node. When env is set it
// is used as-is; otherwise all envs the node participates in are returned.
func statusEnvs(inv repo.Inventory, node, env string) []string {
	if env != "" {
		return []string{env}
	}
	return inv.EnvsForNode(node)
}

// lock takes an exclusive, non-blocking flock on <home>/kompensator.lock so a
// cron tick and a manual/SSH-triggered run never reconcile concurrently.
func lock(home string) (unlock func(), held bool, err error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, false, fmt.Errorf("create home dir: %w", err)
	}
	path := filepath.Join(home, "kompensator.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
