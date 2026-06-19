package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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
	Home   string
	Env    string
	Logger *slog.Logger
}

// Result summarises a reconcile run.
type Result struct {
	Deployed int
	InSync   int
	Skipped  int
	Failed   int
}

// Run performs a single reconcile pass for the node: sync each deployment repo,
// resolve the apps placed on this node, and deploy any that have drifted from
// the desired state. It is the core of Phase 1 and is invoked both by cron and
// manually.
func Run(ctx context.Context, opts Options) (Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	unlock, held, err := lock(opts.Home)
	if err != nil {
		return Result{}, err
	}
	if !held {
		log.Info("another reconcile is in progress, skipping")
		return Result{}, nil
	}
	defer unlock()

	cfg, err := config.Load(opts.Home)
	if err != nil {
		return Result{}, err
	}

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

	running, err := runtime.RunningColors(ctx, node, opts.Env, app.Name, app.Name)
	if err != nil {
		return err
	}

	// If a running slot already serves the desired image, we are in sync. Stop
	// any other (stale) slot that may have been left behind by a prior switch.
	if active, ok := colorServing(running, desiredRef); ok {
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

	alog.Info("drift detected, deploying to idle color",
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
	got, err := runtime.RunningImage(ctx, targetProject, app.Name)
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

// AppStatus is the desired vs. running state of one app on this node.
type AppStatus struct {
	Repo    string
	Env     string
	App     string
	Desired string // "image:tag", or "" if absent from deployment-state
	Running string // "image:tag", or "" if not running
	Color   string // active Blue/Green slot ("blue"/"green"), or "" if not running
}

// runningRef picks the image/color to report for status. It prefers the slot
// serving the desired image; otherwise the first running slot.
func runningRef(running []runtime.ColorState, desired string) (image, color string) {
	for _, cs := range running {
		if cs.Image == desired {
			return cs.Image, cs.Color
		}
	}
	if len(running) > 0 {
		return running[0].Image, running[0].Color
	}
	return "", ""
}

// State summarises the app's status as a short word.
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

// Status collects the desired vs. running state for every app placed on this
// node, without changing anything. It refreshes the repo(s) first when no
// reconcile is currently running; if one is, it reports the last-synced state
// to avoid interfering.
//
// When opts.Env is empty, every environment the node participates in is
// reported; otherwise only the named environment.
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

		envs, err := statusEnvs(dest, cfg.Node.Name, opts.Env)
		if err != nil {
			return out, fmt.Errorf("repo %q: %w", r.Name, err)
		}

		for _, env := range envs {
			apps, state, _, member, err := resolve(dest, cfg.Node.Name, env)
			if err != nil {
				return out, fmt.Errorf("repo %q env %q: %w", r.Name, env, err)
			}
			if !member {
				continue
			}

			for _, app := range apps {
				st := AppStatus{Repo: r.Name, Env: env, App: app.Name}
				if d, ok := state[app.Name]; ok && d.Image != "" && d.Tag != "" {
					st.Desired = d.Ref()
				}
				running, err := runtime.RunningColors(ctx, cfg.Node.Name, env, app.Name, app.Name)
				if err != nil {
					return out, err
				}
				st.Running, st.Color = runningRef(running, st.Desired)
				out = append(out, st)
			}
		}
	}
	return out, nil
}

// statusEnvs returns the environments to report for a repo. When env is set it
// is used as-is; otherwise all envs the node participates in (per the repo's
// inventory) are returned.
func statusEnvs(repoRoot, node, env string) ([]string, error) {
	if env != "" {
		return []string{env}, nil
	}
	inv, err := repo.LoadInventory(repoRoot)
	if err != nil {
		return nil, err
	}
	return inv.EnvsForNode(node), nil
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
