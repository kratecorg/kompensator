package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"kompensator/internal/proxy"
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
	Env     string // empty means: every environment (in the selected repos)
	Repo    string // empty means: every configured repo; else only this repo
	Force   bool   // redeploy even when the desired images are already running
	JSONLog bool   // pass -json through to node agents (controller mode)
	Logger  *slog.Logger
}

// Result summarises a reconcile run, counted per project.
type Result struct {
	Deployed int
	InSync   int
	Skipped  int
	Failed   int
}

// Run performs a reconcile. On a node agent (node.yml) it does a single local
// pass per environment: sync the followed repo, resolve the stacks/projects
// placed on this node, and deploy any that have drifted. On a controller
// (controller.yml) it instead triggers each participating node's agent —
// locally by re-executing itself with the node's home, or remotely over ssh.
//
// When opts.Env is empty it reconciles every environment found in the selected
// repos; when opts.Repo is empty it acts on every configured repo. The home
// lock is held for the whole run so all environments reconcile atomically with
// respect to other runs.
func Run(ctx context.Context, opts Options) (Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	cfg, err := config.Load(opts.Home)
	if err != nil {
		return Result{}, err
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

	envs := []string{opts.Env}
	if opts.Env == "" {
		envs, err = resolveEnvs(ctx, log, opts, cfg)
		if err != nil {
			return Result{}, err
		}
		if len(envs) == 0 {
			log.Info("no environments to reconcile")
			return Result{}, nil
		}
		log.Info("reconciling all environments", "envs", strings.Join(envs, ","))
	}

	var total Result
	for _, env := range envs {
		o := opts
		o.Env = env
		var res Result
		if cfg.IsController() {
			res, err = runController(ctx, log, o, cfg)
		} else {
			res, err = runNode(ctx, log, o, cfg)
		}
		total.add(res)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// filterRepos returns the configured repos, narrowed to one by name when repo
// is non-empty. An unknown name is an error.
func filterRepos(repos []config.Repo, repo string) ([]config.Repo, error) {
	if repo == "" {
		return repos, nil
	}
	for _, r := range repos {
		if r.Name == repo {
			return []config.Repo{r}, nil
		}
	}
	return nil, fmt.Errorf("repo %q not configured", repo)
}

// resolveEnvs syncs the selected repos and returns the union of environments
// they define, deduplicated and sorted.
func resolveEnvs(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) ([]string, error) {
	repos, err := filterRepos(cfg.Repos, opts.Repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var envs []string
	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return nil, fmt.Errorf("sync repo %q: %w", r.Name, err)
		}
		log.Info("repo synced", "repo", r.Name, "commit", commit)
		es, err := repo.ListEnvironments(dest)
		if err != nil {
			return nil, fmt.Errorf("repo %q: %w", r.Name, err)
		}
		for _, e := range es {
			if !seen[e] {
				seen[e] = true
				envs = append(envs, e)
			}
		}
	}
	sort.Strings(envs)
	return envs, nil
}

// runNode is the node-local reconcile pass (cron and manual entrypoint). The
// home lock is already held by Run.
func runNode(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) (Result, error) {
	log = log.With("node", cfg.NodeName, "env", opts.Env)
	log.Info("reconcile started")

	repos, err := filterRepos(cfg.Repos, opts.Repo)
	if err != nil {
		return Result{}, err
	}

	var total Result
	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return total, fmt.Errorf("sync repo %q: %w", r.Name, err)
		}
		log.Info("repo synced", "repo", r.Name, "commit", commit)

		names := runtime.Names{
			Repo: r.Name, Node: cfg.NodeName,
			IncludeRepo: cfg.Naming.UseRepo(), IncludeNode: cfg.Naming.UseNode(),
		}
		res, rerr := reconcileRepo(ctx, log, names, opts, dest)
		total.add(res)

		// Finalise the environment's status document regardless of outcome, so a
		// partial failure is recorded as unhealthy for the pipeline to observe.
		// Publishing to git is gated on the node opting in.
		if err := finalizeNodeStatus(ctx, log, opts, cfg, r, dest, names, commit, total); err != nil {
			log.Error("write status failed", "error", err)
		}
		if rerr != nil {
			return total, fmt.Errorf("reconcile repo %q: %w", r.Name, rerr)
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

// finalizeNodeStatus records the run-level outcome of a node reconcile into the
// environment's status document: the reconciled repo commit, a timestamp, the
// overall health, and a fresh per-service running snapshot. It preserves the
// per-project config fingerprints written incrementally during the reconcile.
//
// Healthy is the deploy signal a pipeline waits on: it is true only when no
// project failed to reconcile (every project either deployed and became
// healthy, was already in sync, or had nothing to deploy). The per-service
// detail is recorded for observability and is not part of that signal.
//
// When the node enables status write-back, the saved document is also published
// to git so a CI pipeline can observe DesiredCommit and Healthy.
func finalizeNodeStatus(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config, r config.Repo, repoRoot string, names runtime.Names, commit string, res Result) error {
	d, err := loadStatusDoc(opts.Home, opts.Env)
	if err != nil {
		return err
	}
	d.Node = cfg.NodeName
	d.Env = opts.Env
	d.DesiredCommit = commit
	d.ReconciledAt = time.Now().UTC().Format(time.RFC3339)
	d.Healthy = res.Failed == 0

	// Refresh the per-service running snapshot. A best-effort collection: if the
	// docker query fails we still persist the run-level fields (commit, health)
	// rather than lose the whole document.
	rows, err := statusEnv(ctx, repoRoot, names, statusTarget{node: cfg.NodeName}, opts.Env)
	if err != nil {
		log.Warn("collect status snapshot failed, recording run-level fields only", "error", err)
	} else {
		byProject := map[string]map[string][]ServiceStatus{}
		for _, r := range rows {
			key := deployKey(r.Stack, r.Project)
			if byProject[key] == nil {
				byProject[key] = map[string][]ServiceStatus{}
			}
			byProject[key][r.Service] = append(byProject[key][r.Service], r)
		}
		for key, svcs := range byProject {
			p := d.project(key)
			p.Services = make(map[string]serviceState, len(svcs))
			for svc, srows := range svcs {
				p.Services[svc] = summarizeService(srows)
			}
		}
	}

	if err := d.save(opts.Home, opts.Env); err != nil {
		return err
	}
	log.Info("status written", "env", opts.Env, "commit", commit, "healthy", d.Healthy)

	if cfg.StatusWriteback {
		if err := publishStatus(ctx, log, opts, cfg, r); err != nil {
			return fmt.Errorf("publish status: %w", err)
		}
	}
	return nil
}

// summarizeService folds the per-container status rows of a single service into
// one observed state: the serving color when in sync, otherwise whatever is
// running, otherwise the desired tag with InSync false.
func summarizeService(rows []ServiceStatus) serviceState {
	for _, r := range rows {
		if r.State() == "in-sync" {
			return serviceState{Tag: refTag(r.Desired), Color: r.Color, Health: r.Health, InSync: true}
		}
	}
	for _, r := range rows {
		if r.Running != "" {
			return serviceState{Tag: refTag(r.Running), Color: r.Color, Health: r.Health, InSync: false}
		}
	}
	st := serviceState{InSync: false}
	for _, r := range rows {
		if r.Desired != "" {
			st.Tag = refTag(r.Desired)
		}
	}
	return st
}

// refTag returns the tag portion of an "image:tag" reference, or the whole
// string when it carries no tag.
func refTag(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// statusKeepCommits bounds the node's status branch to a small rolling window of
// recent reconciles: enough to debug the last few runs, never growing without
// bound.
const statusKeepCommits = 10

// statusBranch is the git branch a node publishes its status to. It derives
// from the deployed branch so a repo can carry several deploy lines, each with
// its own per-node status, e.g. "kompensator-status/customer03". A "-status"
// suffix (rather than nesting under the branch name) is required: git stores
// refs as files, so "kompensator/status/customer03" would collide with the
// existing "kompensator" ref (directory/file conflict).
func statusBranch(deployBranch, node string) string {
	if deployBranch == "" {
		deployBranch = "main"
	}
	return deployBranch + "-status/" + node
}

// gatherStatusFiles reads the node's local status documents, returning them
// keyed by their path on the status branch ("status/<env>.yml"). Publishing the
// whole set keeps the branch a faithful mirror of the node's local status, so
// one env's publish never drops another's.
func gatherStatusFiles(home string) (map[string][]byte, error) {
	dir := statusDir(home)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil
		}
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files["status/"+e.Name()] = data
	}
	return files, nil
}

// publishStatus pushes the node's local status documents to its status branch.
// It is a no-op for content that has not changed since the last publish.
func publishStatus(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config, r config.Repo) error {
	files, err := gatherStatusFiles(opts.Home)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	branch := statusBranch(r.Branch, cfg.NodeName)
	gitDir := filepath.Join(opts.Home, "status-git", r.Name)
	if err := gitsync.PublishStatusBranch(ctx, gitDir, r.URL, branch, files, statusKeepCommits); err != nil {
		return err
	}
	log.Info("status published", "branch", branch)
	return nil
}

// environment. Nodes are processed one at a time so a Blue/Green rollout never
// runs on two nodes simultaneously. A failure on one node is recorded and the
// rest still run. The home lock is already held by Run.
func runController(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) (Result, error) {
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

// reconcileTargets syncs the controller's repos and returns the nodes that run
// at least one stack of the environment, deduplicated by LOCATION (host+path).
// Two repos that share a node name but point at different homes are distinct
// targets; the same home reached via two repos is triggered once. Every such
// node must declare a location so the controller can reach it.
func reconcileTargets(ctx context.Context, log *slog.Logger, opts Options, cfg *config.Config) ([]reconcileTarget, error) {
	repos, err := filterRepos(cfg.Repos, opts.Repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var targets []reconcileTarget
	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return nil, fmt.Errorf("sync repo %q: %w", r.Name, err)
		}
		log.Info("repo synced", "repo", r.Name, "commit", commit)

		env, err := repo.LoadEnvironment(dest, opts.Env)
		if err != nil {
			// This repo does not define the environment; nothing to trigger.
			log.Info("env not defined in repo, skipping", "repo", r.Name, "env", opts.Env)
			continue
		}
		inv, err := repo.LoadInventory(dest)
		if err != nil {
			return nil, fmt.Errorf("repo %q: %w", r.Name, err)
		}
		for _, n := range inv.Nodes {
			if !env.RunsOnNode(n.Name) {
				continue // every stack pinned away from this node
			}
			if n.Location == "" {
				return nil, fmt.Errorf("node %q has no location; controller cannot trigger reconcile", n.Name)
			}
			loc, err := repo.ParseLocation(n.Location)
			if err != nil {
				return nil, fmt.Errorf("node %q: %w", n.Name, err)
			}
			key := loc.String()
			if seen[key] {
				continue
			}
			seen[key] = true
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

	env, err := repo.LoadEnvironment(repoRoot, opts.Env)
	if err != nil {
		// This repo does not define the environment; nothing to do here.
		log.Info("env not defined in this repo, skipping repo", "repo", repoRoot, "env", opts.Env)
		return res, nil
	}
	if !env.RunsOnNode(names.Node) {
		log.Info("node runs no stack of this env, skipping repo", "repo", repoRoot, "node", names.Node)
		return res, nil
	}
	if len(env.Stacks) == 0 {
		log.Info("env hosts no stacks")
		return res, nil
	}

	for _, placement := range env.Stacks {
		stackName := placement.Name
		if !placement.StackRunsOn(names.Node) {
			log.Info("stack not placed on this node, skipping", "stack", stackName, "node", names.Node)
			continue
		}

		// Ensure the stack's reverse-proxy dynamic-config directory exists before
		// its projects deploy: the stack's managed proxy bind-mounts it (PROXY_DIR)
		// and Blue/Green projects write color-switch files into it.
		if err := os.MkdirAll(proxyDir(opts.Home, names.Repo, opts.Env, stackName), 0o755); err != nil {
			return res, fmt.Errorf("create proxy dir: %w", err)
		}

		stack, err := repo.LoadStack(repoRoot, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q: %w", stackName, err)
		}
		if err := validateProxyBindings(stack); err != nil {
			return res, err
		}
		state, err := repo.LoadStackState(repoRoot, opts.Env, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q: %w", stackName, err)
		}
		vars := repo.MergeVariables(stack.Variables, env.Variables, env.NodeVars(names.Node))
		secretVars, err := loadSecretVars(opts.Home, repoRoot, opts.Env, stackName)
		if err != nil {
			return res, fmt.Errorf("stack %q secrets: %w", stackName, err)
		}
		for k, v := range secretVars {
			vars[k] = v
		}
		for _, p := range stack.Projects {
			if !placement.ProjectRunsOn(p.Name, names.Node) {
				log.Info("project not placed on this node, skipping", "stack", stackName, "project", p.Name, "node", names.Node)
				continue
			}
			if err := reconcileProject(ctx, log, names, opts, repoRoot, stackName, p, vars, state[p.Name], &res); err != nil {
				log.Error("project reconcile failed", "stack", stackName, "project", p.Name, "error", err)
				res.Failed++
			}
		}

		// The stack's managed proxy is deployed last: it routes to the projects
		// above, so they (and the network a project owns) must exist first.
		if stack.Proxy != nil {
			if err := reconcileManagedProxy(ctx, log, names, opts, stackName, *stack.Proxy, &res); err != nil {
				log.Error("managed proxy reconcile failed", "stack", stackName, "proxy", stack.Proxy.Name, "error", err)
				res.Failed++
			}
		}
	}
	return res, nil
}

// validateProxyBindings checks that every project proxy binding in the stack
// resolves to the stack's managed proxy. A binding without a managed proxy, or
// targeting a name the stack does not provide, is a configuration error caught
// here rather than producing routes nothing serves.
func validateProxyBindings(stack repo.Stack) error {
	for _, p := range stack.Projects {
		for _, b := range p.Proxy {
			if stack.Proxy == nil {
				return fmt.Errorf("stack %q project %q declares a proxy binding but the stack has no managed proxy", stack.Name, p.Name)
			}
			if t := b.TargetName(); t != stack.Proxy.Name {
				return fmt.Errorf("stack %q project %q proxy binding targets %q but the stack only provides proxy %q", stack.Name, p.Name, t, stack.Proxy.Name)
			}
		}
	}
	return nil
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

// computeConfigHash fingerprints everything that influences a deploy but is not
// captured by the image tags: the compose file content and the effective deploy
// environment (sorted KEY=value entries: variables, secrets and image refs).
// Two deploys with the same fingerprint are equivalent. The secret values feed
// the one-way hash only, so the fingerprint never leaks them.
func computeConfigHash(composeFile string, extraEnv []string) (string, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(data)
	h.Write([]byte{0})
	for _, e := range extraEnv {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// proxyDir is the node-local directory a file-based reverse proxy watches for
// dynamic configuration, scoped per deployment-repo, environment and stack.
// kompensator writes Blue/Green color-switch files here, exposes it to compose
// as PROXY_DIR, and bind-mounts it into the stack's managed proxy. The repo
// segment keeps multiple deployment repos from colliding; the per-stack scope
// keeps stacks fully isolated (one stack can never see or overwrite another's
// routes). A synthesized managed proxy's compose file lives in a ".proxy"
// subdirectory of this path.
func proxyDir(home, repo, env, stack string) string {
	return filepath.Join(home, "proxy", repo, env, stack)
}

// deployKey identifies a project's fingerprint within an environment's status
// document. It is color-independent so a Blue/Green project keeps one entry
// across color switches.
func deployKey(stack, project string) string {
	return stack + "/" + project
}

// readDeployHash returns the last deployed config fingerprint for a project, or
// "" when none was recorded yet (treated as drift so the next reconcile
// re-establishes the baseline). The fingerprint lives in the environment's
// status document.
func readDeployHash(home, env, stack, project string) string {
	d, err := loadStatusDoc(home, env)
	if err != nil {
		return ""
	}
	if p := d.Projects[deployKey(stack, project)]; p != nil {
		return p.ConfigHash
	}
	return ""
}

// writeDeployHash records the config fingerprint of the deploy just performed,
// updating only that project's entry in the environment's status document and
// leaving every other field (and other projects) untouched, so a partial
// reconcile still persists the progress it made.
func writeDeployHash(home, env, stack, project, hash string) error {
	d, err := loadStatusDoc(home, env)
	if err != nil {
		return err
	}
	if d.Env == "" {
		d.Env = env
	}
	d.project(deployKey(stack, project)).ConfigHash = hash
	return d.save(home, env)
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

	extraEnv, desiredRefs := buildEnv(names, opts.Env, stack, p.Name, proxyDir(opts.Home, names.Repo, opts.Env, stack), vars, desired)
	composeFile := repo.ComposeFile(repoRoot, stack, p)

	// The config hash folds the compose file and the effective deploy env
	// (variables, secrets and image refs) into one fingerprint. It lets a
	// reconcile detect changes that leave the image tags untouched — a changed
	// variable, secret or compose file — which image comparison alone misses.
	configHash, err := computeConfigHash(composeFile, extraEnv)
	if err != nil {
		return fmt.Errorf("config hash: %w", err)
	}

	if p.BlueGreen() {
		return blueGreenProject(ctx, plog, names, opts, composeFile, stack, p.Name, extraEnv, desiredRefs, configHash, p.Proxy, res)
	}
	return recreateProject(ctx, plog, names, opts, composeFile, stack, p.Name, extraEnv, desiredRefs, configHash, res)
}

// recreateProject deploys a project in place (no color). Used for projects that
// cannot run two colors at once, e.g. a database.
func recreateProject(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, composeFile, stack, project string, extraEnv []string, desiredRefs map[string]string, configHash string, res *Result) error {
	proj := names.Project(opts.Env, stack, project, "")

	running, err := runtime.ProjectImages(ctx, "", proj)
	if err != nil {
		return err
	}
	imagesInSync := imagesMatch(running, desiredRefs)
	if imagesInSync && readDeployHash(opts.Home, opts.Env, stack, project) == configHash && !opts.Force {
		log.Info("in sync", "images", describeRefs(desiredRefs))
		res.InSync++
		return nil
	}

	reason := "drift detected, recreating project"
	switch {
	case opts.Force:
		reason = "force redeploy (recreate)"
	case imagesInSync:
		reason = "config changed, recreating project"
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
	if err := writeDeployHash(opts.Home, opts.Env, stack, project, configHash); err != nil {
		return err
	}

	log.Info("deployed", "images", describeRefs(desiredRefs))
	res.Deployed++
	return nil
}

// blueGreenProject deploys a project into the idle color, waits for it to
// become healthy, points the environment's reverse proxy at it (when the
// project declares one), then stops the previously active color.
func blueGreenProject(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, composeFile, stack, project string, extraEnv []string, desiredRefs map[string]string, configHash string, proxyBindings []repo.ProxyBinding, res *Result) error {
	running, err := runningByColor(ctx, "", names, opts.Env, stack, project)
	if err != nil {
		return err
	}

	// A color that fully serves the desired images AND matches the deployed
	// config means we are in sync; re-assert the proxy route (idempotent, and
	// repairs a lost dynamic file) and stop any stale other color left behind by
	// a prior switch. The fingerprint is tracked per project, independent of the
	// active color, so a config change forces a switch to the idle color just
	// like an image change. --force overrides.
	active := colorServing(running, desiredRefs)
	if active != "" && readDeployHash(opts.Home, opts.Env, stack, project) == configHash && !opts.Force {
		log.Info("in sync", "images", describeRefs(desiredRefs), "color", active)
		if err := switchProxy(ctx, log, names, opts, stack, project, active, proxyBindings); err != nil {
			return err
		}
		if err := stopOtherColors(ctx, log, names, opts.Env, stack, project, active, running); err != nil {
			return err
		}
		res.InSync++
		return nil
	}

	target := runtime.OtherColor(currentColor(running))
	targetProject := names.Project(opts.Env, stack, project, target)

	reason := "drift detected, deploying to idle color"
	switch {
	case opts.Force:
		reason = "force redeploy to idle color"
	case active != "":
		reason = "config changed, deploying to idle color"
	}
	log.Info(reason, "running", describeColors(running), "desired", describeRefs(desiredRefs), "target_color", target)

	// COLOR lets the compose file tag the new color's container (e.g. a network
	// alias "<service>-<color>") so the reverse proxy can address it. It is not
	// part of the config hash: alternating colors are intended, not drift.
	colorEnv := append(append([]string(nil), extraEnv...), "COLOR="+target)
	if err := runtime.Deploy(ctx, composeFile, targetProject, names.Node, colorEnv); err != nil {
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

	// Cut traffic over to the new color before stopping the old one: the proxy
	// switch is the atomic moment users experience the new version.
	if err := switchProxy(ctx, log, names, opts, stack, project, target, proxyBindings); err != nil {
		return err
	}
	if err := stopOtherColors(ctx, log, names, opts.Env, stack, project, target, running); err != nil {
		return err
	}
	if err := writeDeployHash(opts.Home, opts.Env, stack, project, configHash); err != nil {
		return err
	}

	log.Info("deployed", "images", describeRefs(desiredRefs), "color", target)
	res.Deployed++
	return nil
}

// switchProxy points the environment's reverse proxy at the given color for
// every service the project routes. It is a no-op for projects without a proxy
// binding. For each binding it resolves the active color's replica containers
// and hands them to the Router as concrete backends, so the proxy load-balances
// across every replica.
func switchProxy(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, stack, project, color string, bindings []repo.ProxyBinding) error {
	if len(bindings) == 0 {
		return nil
	}
	colorProject := names.Project(opts.Env, stack, project, color)
	for _, binding := range bindings {
		router, err := proxy.New(binding.Kind)
		if err != nil {
			return fmt.Errorf("proxy %q: %w", binding.Kind, err)
		}
		servers, err := runtime.ServiceEndpoints(ctx, "", colorProject, binding.Service)
		if err != nil {
			return fmt.Errorf("resolve %s replicas: %w", binding.Service, err)
		}
		t := proxy.Target{
			Env:        opts.Env,
			Stack:      stack,
			Project:    project,
			Router:     binding.RouterName(project),
			Service:    binding.Service,
			Port:       binding.Port,
			Servers:    servers,
			Rule:       binding.Rule,
			Color:      color,
			EntryPoint: binding.EntryPoint,
			DynamicDir: proxyDir(opts.Home, names.Repo, opts.Env, stack),
		}
		if err := router.Switch(ctx, t); err != nil {
			return fmt.Errorf("proxy %q switch: %w", binding.Kind, err)
		}
		log.Info("proxy switched", "proxy", binding.Kind, "router", t.Router, "service", binding.Service, "color", color, "replicas", len(servers))
	}
	return nil
}

// reconcileManagedProxy synthesizes and deploys a stack's internal reverse
// proxy. kompensator renders the proxy's compose file from the ManagedProxy
// declaration (the user never writes one), stores it under the stack's
// dynamic-config directory, and deploys it as a recreate project. The deploy
// env is just ENV_NAME — the only variable the generated compose references —
// so stack/env variable churn never recreates the proxy; only a change to the
// declaration itself (networks, publish, dynamic dir) does.
func reconcileManagedProxy(ctx context.Context, log *slog.Logger, names runtime.Names, opts Options, stack string, mp repo.ManagedProxy, res *Result) error {
	project := "proxy-" + mp.Name
	plog := log.With("stack", stack, "project", project, "strategy", "recreate", "proxy", mp.Kind)

	router, err := proxy.New(mp.Kind)
	if err != nil {
		return err
	}
	prov, ok := router.(proxy.Provisioner)
	if !ok {
		return fmt.Errorf("proxy kind %q cannot be managed by kompensator", mp.Kind)
	}

	dynDir := proxyDir(opts.Home, names.Repo, opts.Env, stack)
	nets := make([]proxy.ManagedNetwork, len(mp.Networks))
	for i, n := range mp.Networks {
		aliases := make([]string, len(n.Aliases))
		for j, a := range n.Aliases {
			aliases[j] = expandIdentity(a, names, opts.Env, stack)
		}
		nets[i] = proxy.ManagedNetwork{Name: expandIdentity(n.Name, names, opts.Env, stack), Aliases: aliases}
	}
	composeYAML, err := prov.Compose(proxy.ManagedSpec{
		Name:       mp.Name,
		Env:        opts.Env,
		Stack:      stack,
		DynamicDir: dynDir,
		Networks:   nets,
		Publish:    mp.Publish,
	})
	if err != nil {
		return fmt.Errorf("render managed proxy: %w", err)
	}

	composePath := filepath.Join(dynDir, ".proxy", mp.Name+".yml")
	if err := os.MkdirAll(filepath.Dir(composePath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(composePath, composeYAML, 0o644); err != nil {
		return fmt.Errorf("write managed proxy compose: %w", err)
	}

	proj := names.Project(opts.Env, stack, project, "")
	extraEnv := []string{"ENV_NAME=" + opts.Env}
	configHash, err := computeConfigHash(composePath, extraEnv)
	if err != nil {
		return fmt.Errorf("config hash: %w", err)
	}

	running, err := runtime.ProjectImages(ctx, "", proj)
	if err != nil {
		return err
	}
	if len(running) > 0 && readDeployHash(opts.Home, opts.Env, stack, project) == configHash && !opts.Force {
		plog.Info("in sync")
		res.InSync++
		return nil
	}

	reason := "managed proxy drift, deploying"
	switch {
	case opts.Force:
		reason = "force redeploy managed proxy"
	case len(running) > 0:
		reason = "managed proxy config changed, recreating"
	}
	plog.Info(reason)

	if err := runtime.Deploy(ctx, composePath, proj, names.Node, extraEnv); err != nil {
		return err
	}
	if err := runtime.WaitHealthy(ctx, proj, healthTimeout); err != nil {
		return fmt.Errorf("managed proxy not healthy: %w", err)
	}
	if err := writeDeployHash(opts.Home, opts.Env, stack, project, configHash); err != nil {
		return err
	}
	plog.Info("deployed")
	res.Deployed++
	return nil
}

// expandIdentity substitutes the stack-scoped built-in variables in a string
// taken from stack.yml, e.g. a managed proxy's network name or alias. Variables
// in compose files are expanded by docker compose at deploy time, but stack.yml
// is parsed by kompensator itself, so kompensator resolves these here. Only the
// variables known before a concrete project/color exists are available (no
// PROJECT_PREFIX/COLOR); an unknown variable expands to empty, as in a shell.
func expandIdentity(s string, names runtime.Names, env, stack string) string {
	repl := map[string]string{
		"ENV_NAME":     env,
		"STACK_NAME":   stack,
		"STACK_PREFIX": names.StackPrefix(env, stack),
		"NODE_NAME":    names.Node,
		"REPO_NAME":    names.Repo,
	}
	return os.Expand(s, func(k string) string { return repl[k] })
}

// buildEnv assembles the compose env vars for a deploy: the stack/env variables
// (lowest precedence), the built-in identity vars (ENV_NAME, REPO_NAME,
// STACK_NAME, PROJECT_NAME, STACK_PREFIX, PROJECT_PREFIX, PROXY_DIR) and the
// per-service <SERVICE>_IMAGE / <SERVICE>_TAG values (highest precedence, so user
// variables can never break image injection). NODE_NAME is added separately by
// Deploy, and COLOR by blueGreenProject. STACK_PREFIX is shared by every project
// of a stack and PROJECT_PREFIX is the color-independent compose project name, so
// a compose file can name a resource (e.g. a shared network) stably across
// projects of a stack or across color switches. It also returns the desired
// image ref per service.
func buildEnv(names runtime.Names, envName, stack, project, proxyDir string, vars map[string]string, desired map[string]repo.ServiceImage) (extraEnv []string, refs map[string]string) {
	merged := make(map[string]string, len(vars)+len(desired)*2+8)
	for k, v := range vars {
		merged[k] = v
	}
	merged["ENV_NAME"] = envName
	merged["STACK_NAME"] = stack
	merged["PROJECT_NAME"] = project
	// STACK_PREFIX is the name prefix shared by every project of this stack
	// (color-independent and project-independent), so one project can own a
	// resource (e.g. a network) and another project of the same stack can
	// reference it by the same stable name.
	merged["STACK_PREFIX"] = names.StackPrefix(envName, stack)
	// PROJECT_PREFIX is the deploy's compose project name without the Blue/Green
	// color suffix: globally unique on the host yet stable across color switches,
	// so it is the safe handle for naming color-independent resources.
	merged["PROJECT_PREFIX"] = names.Project(envName, stack, project, "")
	if names.Repo != "" {
		merged["REPO_NAME"] = names.Repo
	}
	// PROXY_DIR is the node-local directory a file-based reverse proxy watches
	// for dynamic configuration. The proxy stack bind-mounts it; kompensator
	// writes color-switch files into it (see internal/proxy).
	merged["PROXY_DIR"] = proxyDir

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

	repos, err := filterRepos(cfg.Repos, opts.Repo)
	if err != nil {
		return nil, err
	}

	var out []ServiceStatus
	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		if canPull {
			if _, err := gitsync.Sync(ctx, r.URL, r.Branch, dest); err != nil {
				return out, fmt.Errorf("sync repo %q: %w", r.Name, err)
			}
		}

		var targets []statusTarget
		if cfg.IsController() {
			inv, err := repo.LoadInventory(dest)
			if err != nil {
				return out, fmt.Errorf("repo %q: %w", r.Name, err)
			}
			targets, err = statusTargets(inv, cfg.NodeName)
			if err != nil {
				return out, fmt.Errorf("repo %q: %w", r.Name, err)
			}
		} else {
			if cfg.NodeName == "" {
				return out, fmt.Errorf("node config has no node name")
			}
			targets = []statusTarget{{node: cfg.NodeName}}
		}

		envs, err := statusEnvs(dest, opts.Env)
		if err != nil {
			return out, fmt.Errorf("repo %q: %w", r.Name, err)
		}
		for _, t := range targets {
			names := runtime.Names{
				Repo: r.Name, Node: t.node,
				IncludeRepo: cfg.Naming.UseRepo(), IncludeNode: cfg.Naming.UseNode(),
			}
			for _, env := range envs {
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
// one node. It is a no-op when the node runs no stack of the environment.
func statusEnv(ctx context.Context, repoRoot string, names runtime.Names, t statusTarget, env string) ([]ServiceStatus, error) {
	e, err := repo.LoadEnvironment(repoRoot, env)
	if err != nil {
		return nil, fmt.Errorf("repo %q env %q: %w", names.Repo, env, err)
	}
	if !e.RunsOnNode(t.node) {
		return nil, nil // every stack pinned away from this node
	}

	var out []ServiceStatus
	for _, placement := range e.Stacks {
		if !placement.StackRunsOn(t.node) {
			continue // stack pinned away from this node
		}
		stackName := placement.Name
		stack, err := repo.LoadStack(repoRoot, stackName)
		if err != nil {
			return nil, fmt.Errorf("repo %q stack %q: %w", names.Repo, stackName, err)
		}
		state, err := repo.LoadStackState(repoRoot, env, stackName)
		if err != nil {
			return nil, fmt.Errorf("repo %q stack %q: %w", names.Repo, stackName, err)
		}
		for _, p := range stack.Projects {
			if !placement.ProjectRunsOn(p.Name, t.node) {
				continue // project pinned away from this node
			}
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

// statusEnvs returns the environments to report. When env is set it is used
// as-is; otherwise every environment defined in the repo is returned.
func statusEnvs(repoRoot, env string) ([]string, error) {
	if env != "" {
		return []string{env}, nil
	}
	return repo.ListEnvironments(repoRoot)
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
