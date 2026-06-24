// Package verify checks, purely from git, whether a deployment has reached a
// desired commit and become healthy across the nodes that host an environment.
//
// It is the read side of the node status write-back: every participating node
// publishes its reconcile status to a branch (see internal/reconcile), and
// verify aggregates those branches without any ssh access to the nodes. This
// lets a CI pipeline that only has git credentials wait for a deployment to
// complete: it pushes a desired-state commit, then polls verify until every
// node reports that commit as healthy.
package verify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kompensator/internal/config"
	"kompensator/internal/repo"

	"gopkg.in/yaml.v3"
)

// Options controls a verify run.
type Options struct {
	RepoPath string        // a checkout of the deployment repo (origin = the remote holding the status branches)
	Env      string        // the environment to verify
	Commit   string        // desired commit; empty means the repo checkout's HEAD
	Branch   string        // deployed branch the status branches nest under; empty means the checkout's current branch
	Wait     bool          // poll until healthy or Timeout
	Timeout  time.Duration // overall budget when Wait is set
	Interval time.Duration // delay between polls when Wait is set
	Logger   *slog.Logger
}

// NodeResult is one node's contribution to a verify run.
type NodeResult struct {
	Node          string
	OK            bool   // published, on the desired commit, and healthy
	DesiredCommit string // commit the node reports as reconciled ("" if unpublished)
	Healthy       bool
	Reason        string // why the node is not OK (empty when OK)
}

// Result is the outcome of a verify run.
type Result struct {
	Env    string
	Commit string // the desired commit verified against
	Nodes  []NodeResult
	OK     bool // every participating node is OK
}

// nodeStatus is the minimal shape verify reads from a node's published
// status/<env>.yml. It intentionally mirrors only the fields the pipeline
// contract depends on.
type nodeStatus struct {
	DesiredCommit string `yaml:"desiredCommit"`
	Healthy       bool   `yaml:"healthy"`
}

// HomeOptions configures a controller/node-side verify that resolves the
// deployment-repo checkout(s) from a kompensator home, the way reconcile does,
// instead of taking a bare repo path. It is the entry point for an operator
// running `kompensator verify <env>` on a controller.
type HomeOptions struct {
	Home     string // kompensator home (controller.yml or node.yml)
	Repo     string // optional repo-name filter
	Env      string
	Commit   string // desired commit; empty means the repo's remote branch tip
	Wait     bool
	Timeout  time.Duration
	Interval time.Duration
	Logger   *slog.Logger
}

// RunHome resolves the repo that hosts the environment from the home config,
// refreshes the desired commit from its remote branch, and verifies every node
// that hosts the environment. Unlike Run (which a CI pipeline points at a bare
// checkout), this needs only a configured kompensator home.
func RunHome(ctx context.Context, opts HomeOptions) (Result, error) {
	cfg, err := config.Load(opts.Home)
	if err != nil {
		return Result{}, err
	}
	repos := cfg.Repos
	if opts.Repo != "" {
		repos = nil
		for _, r := range cfg.Repos {
			if r.Name == opts.Repo {
				repos = append(repos, r)
			}
		}
		if len(repos) == 0 {
			return Result{}, fmt.Errorf("repo %q not configured", opts.Repo)
		}
	}

	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		nodes, err := participatingNodes(dest, opts.Env)
		if err != nil || len(nodes) == 0 {
			continue // this repo does not define the environment
		}
		commit := opts.Commit
		if commit == "" {
			branch := r.Branch
			if branch == "" {
				branch = "main"
			}
			if _, err := git(ctx, dest, "fetch", "-q", "origin", branch); err != nil {
				return Result{}, fmt.Errorf("fetch %s: %w", branch, err)
			}
			c, err := git(ctx, dest, "rev-parse", "FETCH_HEAD")
			if err != nil {
				return Result{}, fmt.Errorf("resolve %s: %w", branch, err)
			}
			commit = strings.TrimSpace(c)
		}
		return Run(ctx, Options{
			RepoPath: dest,
			Env:      opts.Env,
			Commit:   commit,
			Branch:   r.Branch,
			Wait:     opts.Wait,
			Timeout:  opts.Timeout,
			Interval: opts.Interval,
			Logger:   opts.Logger,
		})
	}
	return Result{}, fmt.Errorf("no configured repo hosts environment %q", opts.Env)
}

// Run verifies the environment once, or polls until healthy when opts.Wait is
// set. It returns the final Result; a non-nil error indicates verify could not
// run (bad repo path, unreadable inventory), not an unhealthy deployment.
func Run(ctx context.Context, opts Options) (Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}

	commit := opts.Commit
	if commit == "" {
		c, err := git(ctx, opts.RepoPath, "rev-parse", "HEAD")
		if err != nil {
			return Result{}, fmt.Errorf("resolve HEAD: %w", err)
		}
		commit = strings.TrimSpace(c)
	}
	branch := opts.Branch
	if branch == "" {
		b, err := git(ctx, opts.RepoPath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return Result{}, fmt.Errorf("resolve branch: %w", err)
		}
		branch = strings.TrimSpace(b)
	}

	nodes, err := participatingNodes(opts.RepoPath, opts.Env)
	if err != nil {
		return Result{}, err
	}
	if len(nodes) == 0 {
		return Result{}, fmt.Errorf("no node hosts environment %q", opts.Env)
	}

	deadline := time.Now().Add(opts.Timeout)
	for {
		res := check(ctx, opts.RepoPath, branch, opts.Env, commit, nodes)
		if res.OK || !opts.Wait {
			return res, nil
		}
		if time.Now().After(deadline) {
			log.Info("verify timed out", "env", opts.Env, "commit", commit)
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(opts.Interval):
		}
	}
}

// check inspects every node's status branch once.
func check(ctx context.Context, repoPath, branch, env, commit string, nodes []string) Result {
	res := Result{Env: env, Commit: commit, OK: true}
	for _, node := range nodes {
		nr := NodeResult{Node: node}
		st, err := fetchNodeStatus(ctx, repoPath, branch, node, env)
		switch {
		case err != nil:
			nr.Reason = "no published status"
		default:
			nr.DesiredCommit = st.DesiredCommit
			nr.Healthy = st.Healthy
			switch {
			case st.DesiredCommit != commit:
				nr.Reason = fmt.Sprintf("on %s, want %s", short(st.DesiredCommit), short(commit))
			case !st.Healthy:
				nr.Reason = "unhealthy"
			default:
				nr.OK = true
			}
		}
		if !nr.OK {
			res.OK = false
		}
		res.Nodes = append(res.Nodes, nr)
	}
	return res
}

// fetchNodeStatus fetches a node's status branch and reads its status/<env>.yml
// without disturbing the working tree.
func fetchNodeStatus(ctx context.Context, repoPath, branch, node, env string) (nodeStatus, error) {
	statusBranch := branch + "-status/" + node
	ref := "refs/kompensator-verify/" + node
	if _, err := git(ctx, repoPath, "fetch", "-q", "--force", "origin", statusBranch+":"+ref); err != nil {
		return nodeStatus{}, fmt.Errorf("fetch %s: %w", statusBranch, err)
	}
	out, err := git(ctx, repoPath, "show", ref+":status/"+env+".yml")
	if err != nil {
		return nodeStatus{}, fmt.Errorf("read status: %w", err)
	}
	var st nodeStatus
	if err := yaml.Unmarshal([]byte(out), &st); err != nil {
		return nodeStatus{}, fmt.Errorf("parse status: %w", err)
	}
	return st, nil
}

// participatingNodes returns the inventory nodes that host at least one stack of
// the environment — the nodes expected to publish a status for it.
func participatingNodes(repoPath, env string) ([]string, error) {
	inv, err := repo.LoadInventory(repoPath)
	if err != nil {
		return nil, fmt.Errorf("load inventory: %w", err)
	}
	e, err := repo.LoadEnvironment(repoPath, env)
	if err != nil {
		return nil, fmt.Errorf("load environment %q: %w", env, err)
	}
	var nodes []string
	for _, n := range inv.AllNodeNames() {
		if e.RunsOnNode(n) {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
