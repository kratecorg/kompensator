package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kompensator/internal/config"
	"kompensator/internal/gitsync"
	"kompensator/internal/placement"
	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// healthTimeout bounds how long a Blue/Green switch waits for the new color to
// become healthy before giving up and reporting the app as failed.
const healthTimeout = 5 * time.Minute

// Options controls a reconcile run.
type Options struct {
	Home    string
	Env     string
	Force   bool // redeploy even when the desired image is already running
	JSONLog bool // pass -json through to node agents (controller mode)
	Logger  *slog.Logger
}

// Result summarises a reconcile run.
type Result struct {
	Deployed int
	InSync   int
	Skipped  int
	Failed   int
}

// Run performs a reconcile. On a node agent (config has node.name) it does a
// single local pass: sync each deployment repo, resolve the apps placed on
// this node, and deploy any that have drifted. On a controller (config has no
// node.name) it instead triggers each participating node's agent — locally by
// re-executing itself with the node's home, or remotely over ssh.
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

		res, err := reconcileRepo(ctx, log, cfg.Node.Name, opts, dest)
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
		return total, fmt.Errorf("%d app(s) failed to reconcile", total.Failed)
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
			if _, ok := inv.RolesFor(n.Name, opts.Env); !ok {
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
// the "kompensator" binary is expected on PATH. The agent's own logs stream to
// the controller's stderr.
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
		sshArgs := []string{"-o", "BatchMode=yes"}
		if loc.Port != "" {
			sshArgs = append(sshArgs, "-p", loc.Port)
		}
		target := loc.Host
		if loc.User != "" {
			target = loc.User + "@" + loc.Host
		}
		remote := append([]string{"kompensator"}, append(global, sub...)...)
		sshArgs = append(sshArgs, target, strings.Join(remote, " "))
		cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func reconcileRepo(ctx context.Context, log *slog.Logger, node string, opts Options, repoRoot string) (Result, error) {
	var res Result

	apps, state, envDir, member, err := resolve(repoRoot, node, opts.Env)
	if err != nil {
		return res, err
	}
	if !member {
		log.Info("node not in this repo's env, skipping repo", "repo", repoRoot)
		return res, nil
	}
	if len(apps) == 0 {
		log.Info("no apps placed on this node for env")
		return res, nil
	}

	for _, app := range apps {
		if err := reconcileApp(ctx, log, node, opts, envDir, app, state, &res); err != nil {
			log.Error("app reconcile failed", "app", app.Name, "error", err)
			res.Failed++
		}
	}
	return res, nil
}

// resolve loads the inventory, placement and desired state for a repo and
// returns the apps placed on this node for the environment. member is false
// when the node does not participate in the environment (or the env has no
// placement), in which case the repo is skipped.
func resolve(repoRoot, node, env string) (apps []repo.App, state map[string]repo.DesiredApp, envDir string, member bool, err error) {
	inv, err := repo.LoadInventory(repoRoot)
	if err != nil {
		return nil, nil, "", false, err
	}

	roles, ok := inv.RolesFor(node, env)
	if !ok {
		return nil, nil, "", false, nil
	}

	pl, err := repo.LoadPlacement(repoRoot, env)
	if err != nil {
		if os.IsNotExist(underlying(err)) {
			return nil, nil, "", false, nil
		}
		return nil, nil, "", false, err
	}

	state, err = repo.LoadDesiredState(repoRoot, env)
	if err != nil {
		return nil, nil, "", false, err
	}

	return placement.AppsForNode(pl, roles), state, repo.EnvDir(repoRoot, env), true, nil
}

func reconcileApp(ctx context.Context, log *slog.Logger, node string, opts Options, envDir string, app repo.App, state map[string]repo.DesiredApp, res *Result) error {
	alog := log.With("app", app.Name)

	desired, ok := state[app.Name]
	if !ok || desired.Image == "" || desired.Tag == "" {
		alog.Warn("no desired image/tag in deployment-state, skipping")
		res.Skipped++
		return nil
	}
	desiredRef := desired.Ref()

	running, err := runtime.RunningColors(ctx, "", node, opts.Env, app.Name, app.Name)
	if err != nil {
		return err
	}

	// If a running slot already serves the desired image, we are in sync. Stop
	// any other (stale) slot that may have been left behind by a prior switch.
	// --force overrides this and triggers a fresh deploy into the idle slot.
	if active, ok := colorServing(running, desiredRef); ok && !opts.Force {
		alog.Info("in sync", "image", desiredRef, "color", active)
		if err := stopOtherColors(ctx, alog, node, opts.Env, app.Name, active, running); err != nil {
			return err
		}
		res.InSync++
		return nil
	}

	// Blue/Green switch: deploy the desired image into the idle slot, wait for
	// it to become fully healthy, verify it, then stop the previously active
	// slot(s).
	target := runtime.OtherColor(currentColor(running))
	targetProject := runtime.ProjectName(node, opts.Env, app.Name, target)

	reason := "drift detected, deploying to idle color"
	if opts.Force {
		reason = "force redeploy to idle color"
	}
	alog.Info(reason,
		"running", describeColors(running),
		"desired", desiredRef,
		"target_color", target,
	)

	composeFile := filepath.Join(envDir, app.Compose)
	if err := runtime.Deploy(ctx, composeFile, targetProject, node, desired.Image, desired.Tag); err != nil {
		return err
	}

	alog.Info("waiting for new color to become healthy", "color", target, "project", targetProject)
	if err := runtime.WaitHealthy(ctx, targetProject, healthTimeout); err != nil {
		return fmt.Errorf("new color %q not healthy: %w", target, err)
	}

	// Verify the new slot actually serves the desired image before cutting over.
	got, err := runtime.RunningImage(ctx, "", targetProject, app.Name)
	if err != nil {
		return err
	}
	if got != desiredRef {
		return fmt.Errorf("after deploy, running image %q != desired %q", got, desiredRef)
	}
	alog.Info("new color healthy", "color", target, "image", desiredRef)

	// New color is live and healthy: stop the old color(s).
	if err := stopOtherColors(ctx, alog, node, opts.Env, app.Name, target, running); err != nil {
		return err
	}

	alog.Info("deployed", "image", desiredRef, "color", target)
	res.Deployed++
	return nil
}

// colorServing returns the running color whose image matches ref, if any.
func colorServing(running []runtime.ColorState, ref string) (string, bool) {
	for _, cs := range running {
		if cs.Image == ref {
			return cs.Color, true
		}
	}
	return "", false
}

// currentColor returns the color to treat as currently active, or "" when
// nothing is running (so the first deploy lands in blue).
func currentColor(running []runtime.ColorState) string {
	if len(running) == 0 {
		return ""
	}
	return running[0].Color
}

// stopOtherColors tears down every running slot except keep.
func stopOtherColors(ctx context.Context, log *slog.Logger, node, env, app, keep string, running []runtime.ColorState) error {
	for _, cs := range running {
		if cs.Color == keep {
			continue
		}
		project := runtime.ProjectName(node, env, app, cs.Color)
		log.Info("stopping old color", "color", cs.Color, "project", project)
		if err := runtime.Stop(ctx, project); err != nil {
			return err
		}
	}
	return nil
}

// describeColors renders the running slots as "blue=img,green=img" for logs.
func describeColors(running []runtime.ColorState) string {
	if len(running) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(running))
	for _, cs := range running {
		parts = append(parts, cs.Color+"="+cs.Image)
	}
	return strings.Join(parts, ",")
}

func (r *Result) add(o Result) {
	r.Deployed += o.Deployed
	r.InSync += o.InSync
	r.Skipped += o.Skipped
	r.Failed += o.Failed
}

// AppStatus is the desired state of one app on one node and, for each running
// replica, the container that serves it. One AppStatus is emitted per running
// container; an app with nothing running yields a single row with empty
// Container/Running.
type AppStatus struct {
	Node      string
	Repo      string
	Env       string
	App       string
	Desired   string // "image:tag", or "" if absent from deployment-state
	Color     string // Blue/Green slot of this container, or "" if not running
	Container string // short container name, e.g. "web-2", or "" if not running
	Running   string // "image:tag" of this container, or "" if not running
	Health    string // docker's human status, e.g. "Up 2 minutes (healthy)"
}

// State summarises the row's status as a short word.
func (s AppStatus) State() string {
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

// Status collects the desired vs. running state for apps, without changing
// anything. It refreshes the repo(s) first when no reconcile is currently
// running; if one is, it reports the last-synced state to avoid interfering.
//
// When the inventory declares node locations, Status acts as a controller and
// aggregates every node (querying each node's docker daemon via its location).
// Otherwise it reports only this node, against the local daemon. When opts.Env
// is empty, every environment a node participates in is reported; otherwise
// only the named environment.
func Status(ctx context.Context, opts Options) ([]AppStatus, error) {
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

	var out []AppStatus
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
			envs := statusEnvs(inv, t.node, opts.Env)
			for _, env := range envs {
				apps, state, _, member, err := resolve(dest, t.node, env)
				if err != nil {
					return out, fmt.Errorf("repo %q node %q env %q: %w", r.Name, t.node, env, err)
				}
				if !member {
					continue
				}

				for _, app := range apps {
					desired := ""
					if d, ok := state[app.Name]; ok && d.Image != "" && d.Tag != "" {
						desired = d.Ref()
					}
					containers, err := runtime.RunningContainers(ctx, t.dockerHost, t.node, env, app.Name, app.Name)
					if err != nil {
						return out, fmt.Errorf("node %q: %w", t.node, err)
					}

					base := AppStatus{Node: t.node, Repo: r.Name, Env: env, App: app.Name, Desired: desired}
					if len(containers) == 0 {
						out = append(out, base)
						continue
					}
					for _, c := range containers {
						st := base
						st.Color = c.Color
						st.Container = c.Name
						st.Running = c.Image
						st.Health = c.Health
						out = append(out, st)
					}
				}
			}
		}
	}
	return out, nil
}

// statusTarget is one node to report on, with the docker endpoint to query.
type statusTarget struct {
	node       string
	dockerHost string // "" = local daemon
}

// statusTargets decides which nodes to report. When the inventory declares
// node locations we are a controller: report every node, querying each via its
// location's docker endpoint (the self node, matching selfNode, uses the local
// daemon to avoid an ssh hop to itself). Without locations we report only the
// configured node against the local daemon (legacy single-node behaviour).
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
// is used as-is; otherwise all envs the node participates in (per the
// inventory) are returned.
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

// underlying unwraps a fmt.Errorf-wrapped error one level for os.IsNotExist.
func underlying(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		if inner := u.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
