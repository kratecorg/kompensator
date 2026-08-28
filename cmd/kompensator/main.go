package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kratecorg/kompensator/internal/admin"
	"github.com/kratecorg/kompensator/internal/check"
	"github.com/kratecorg/kompensator/internal/config"
	"github.com/kratecorg/kompensator/internal/reconcile"
	"github.com/kratecorg/kompensator/internal/verify"
	"github.com/kratecorg/kompensator/internal/version"
)

// buildVersion is the release tag, injected at build time with
// -ldflags "-X main.buildVersion=<tag>". Empty in plain dev builds, where the
// version is instead derived from the embedded VCS build info.
var buildVersion = ""

// globals holds flags shared by all subcommands. They must be given before the
// subcommand, e.g.  kompensator -json -home /path reconcile dev
type globals struct {
	home    string
	jsonLog bool
}

func main() {
	gfs := flag.NewFlagSet("kompensator", flag.ContinueOnError)
	var g globals
	gfs.StringVar(&g.home, "home", "", "kompensator home directory (default: $KOMPENSATOR_HOME or ~/.config/kompensator)")
	gfs.BoolVar(&g.jsonLog, "json", false, "emit structured JSON logs (for journald/Loki)")
	gfs.Usage = usage
	if err := gfs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	args := gfs.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "reconcile":
		os.Exit(cmdReconcile(g, rest))
	case "pause":
		os.Exit(cmdPause(g, rest))
	case "resume":
		os.Exit(cmdResume(g, rest))
	case "status":
		os.Exit(cmdStatus(g, rest))
	case "verify":
		os.Exit(cmdVerify(g, rest))
	case "check":
		os.Exit(cmdCheck(g, rest))
	case "controller":
		os.Exit(cmdController(g, rest))
	case "node":
		os.Exit(cmdNode(g, rest))
	case "bootstrap":
		fmt.Fprintln(os.Stderr, "error: 'bootstrap' is now 'kompensator node add <name> <location>'")
		os.Exit(2)
	case "secrets":
		os.Exit(cmdSecrets(g, rest))
	case "version":
		fmt.Println("kompensator", version.Current(buildVersion).Token())
	case "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func cmdReconcile(g globals, args []string) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	force := fs.Bool("force", false, "redeploy even when the desired image is already running")
	prune := fs.Bool("prune", false, "tear down kompensator-managed containers no longer placed here (containers only; volumes are left intact)")
	repoName := fs.String("repo", "", "limit to this repo (default: all configured repos)")
	ignorePause := fs.Bool("ignore-pause", false, "run even while this home is paused; the pause itself is left in place")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] reconcile [--force] [--prune] [--ignore-pause] [--repo <name>] [<env> [<stack> [<project>]]]")
		fmt.Fprintln(os.Stderr, "  Each argument narrows the run: no <env> reconciles every environment,")
		fmt.Fprintln(os.Stderr, "  <stack> only that stack, <project> only that project of it.")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 3 {
		fs.Usage()
		return 2
	}
	env, stack, project := arg(pos, 0), arg(pos, 1), arg(pos, 2)
	if stack != "" && env == "" {
		fmt.Fprintln(os.Stderr, "error: <stack> needs an <env> in front of it")
		return 2
	}
	if project != "" && stack == "" {
		fmt.Fprintln(os.Stderr, "error: <project> needs a <stack> in front of it")
		return 2
	}
	// Prune is the one part of a reconcile that ignores the narrowing: it tears
	// down every managed project the desired state no longer places on this node,
	// across all stacks. A run that promised to touch one stack cannot also do
	// that.
	if *prune && stack != "" {
		fmt.Fprintln(os.Stderr, "error: --prune acts on the whole environment and cannot be combined with <stack>/<project>")
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if _, err := reconcile.Run(ctx, reconcile.Options{
		Home:        h,
		Env:         env,
		Repo:        *repoName,
		Stack:       stack,
		Project:     project,
		Force:       *force,
		Prune:       *prune,
		IgnorePause: *ignorePause,
		JSONLog:     g.jsonLog,
		Logger:      log,
	}); err != nil {
		log.Error("reconcile failed", "error", err)
		return 1
	}
	return 0
}

// cmdPause suspends reconciling on this home until resumed or until the pause
// expires. It exists so an operation that must not be interrupted — a database
// switchover, for instance — can keep the cron tick out through a declared
// interface instead of by holding kompensator's run lock from outside.
func cmdPause(g globals, args []string) int {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	reason := fs.String("reason", "", "why reconciling is suspended (shown in status and in the reconcile log)")
	timeout := fs.Duration("timeout", 0, "lift the pause automatically after this long; 0 means until resumed")
	wait := fs.Duration("wait", 0, "wait this long for a reconcile already in progress to finish")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] pause [--reason <text>] [--timeout <d>] [--wait <d>]")
		fs.PrintDefaults()
	}
	if _, err := parseFlagsAndArgs(fs, args); err != nil {
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	p, err := reconcile.SetPause(h, *reason, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("paused:", p.Describe(time.Now()))

	// The marker alone only keeps later runs out. Waiting for the lock proves
	// that no run is still underway, which is what a caller needs before it
	// starts changing things.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := reconcile.WaitForIdle(ctx, h, *wait); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "the pause is set; run 'kompensator resume' if you are not going ahead")
		return 1
	}
	fmt.Println("no reconcile in progress")
	return 0
}

// cmdResume lifts a pause. Resuming a home that is not paused is not an error,
// so it can be repeated after an operation that failed halfway.
func cmdResume(g globals, args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] resume")
	}
	if _, err := parseFlagsAndArgs(fs, args); err != nil {
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	lifted, err := reconcile.ClearPause(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !lifted {
		fmt.Println("not paused")
		return 0
	}
	fmt.Println("resumed")
	return 0
}

func cmdStatus(g globals, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	repoName := fs.String("repo", "", "limit to this repo (default: all configured repos)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] status [--repo <name>] [<env>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 1 {
		fs.Usage()
		return 2
	}
	env := arg(pos, 0) // empty means: all environments

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A pause that nobody lifted looks exactly like a healthy node otherwise:
	// nothing drifts because nothing is deployed. It has to be visible here.
	if p, found, err := reconcile.ReadPause(h); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	} else if found && !p.IsExpired(time.Now()) {
		fmt.Printf("RECONCILING IS PAUSED: %s\n\n", p.Describe(time.Now()))
	}

	statuses, err := reconcile.Status(ctx, reconcile.Options{Home: h, Env: env, Repo: *repoName})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(statuses) == 0 {
		if env == "" {
			fmt.Println("No apps placed on this node in any environment.")
		} else {
			fmt.Printf("No apps placed on this node for env %q.\n", env)
		}
		return 0
	}

	drift := 0
	orphans := 0
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tENV\tSTACK\tPROJECT\tSERVICE\tCOLOR\tCONTAINER\tTARGET\tRUNNING\tHEALTH\tSTATUS")
	for _, s := range statuses {
		state := s.State()
		switch state {
		case "drift", "missing":
			drift++
		case "orphan":
			orphans++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(s.Node), s.Env, s.Stack, s.Project, s.Service, dash(s.Color), dash(s.Container),
			dash(s.Desired), dash(s.Running), dash(s.Health), state)
	}
	tw.Flush()

	if orphans > 0 {
		fmt.Printf("\n%d managed container(s) no longer placed here (orphans). "+
			"Their volumes are left intact; remove them manually.\n", orphans)
	}
	if drift > 0 {
		fmt.Printf("\n%d container(s) drifting from target.\n", drift)
	}
	if drift > 0 || orphans > 0 {
		return 1
	}
	return 0
}

func resolveHome(home string) (string, error) {
	if home != "" {
		return home, nil
	}
	return config.Home()
}

// cmdVerify checks, from git, whether every node that hosts an environment has
// reconciled a desired commit and reports healthy.
//
// On a controller (or node) home it resolves the deployment-repo checkout and
// participating nodes from the home config, the way reconcile does, and verifies
// the latest commit on the deploy branch. With --repo-path it instead works off
// a bare checkout with no home and no ssh access, so a CI pipeline can wait for a
// deployment to land.
func cmdVerify(g globals, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	repoPath := fs.String("repo-path", "", "verify against this bare deployment-repo checkout instead of the home config (for CI)")
	repoName := fs.String("repo", "", "limit to this repo (home mode; default: all configured repos)")
	commit := fs.String("commit", "", "desired commit to verify (default: the deploy branch's tip)")
	branch := fs.String("branch", "", "deployed branch the status branches derive from (--repo-path mode; default: the checkout's current branch)")
	wait := fs.Bool("wait", false, "poll until healthy or --timeout elapses")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall budget when --wait is set")
	interval := fs.Duration("interval", 10*time.Second, "delay between polls when --wait is set")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] verify <env> [--repo <name>] [--commit <sha>] [--wait [--timeout <d>] [--interval <d>]]")
		fmt.Fprintln(os.Stderr, "       kompensator verify <env> --repo-path <dir> [--commit <sha>] [--branch <name>] [--wait ...]   (CI, no home)")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	env := arg(pos, 0)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var res verify.Result
	switch {
	case *repoPath != "":
		// CI mode: a bare checkout, no home.
		res, err = verify.Run(ctx, verify.Options{
			RepoPath: *repoPath,
			Env:      env,
			Commit:   *commit,
			Branch:   *branch,
			Wait:     *wait,
			Timeout:  *timeout,
			Interval: *interval,
			Logger:   newLogger(g.jsonLog),
		})
	default:
		// Home mode: resolve the repo checkout(s) from the controller/node config.
		h, herr := resolveHome(g.home)
		if herr != nil || !config.IsOccupied(h) {
			fmt.Fprintln(os.Stderr, "error: no kompensator home found; run in a controller/node home or pass --repo-path <dir>")
			return 1
		}
		res, err = verify.RunHome(ctx, verify.HomeOptions{
			Home:     h,
			Repo:     *repoName,
			Env:      env,
			Commit:   *commit,
			Wait:     *wait,
			Timeout:  *timeout,
			Interval: *interval,
			Logger:   newLogger(g.jsonLog),
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tCOMMIT\tHEALTHY\tSTATUS")
	for _, n := range res.Nodes {
		state := "ok"
		if !n.OK {
			state = n.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n", n.Node, dash(shortSHA(n.DesiredCommit)), n.Healthy, state)
	}
	tw.Flush()

	if !res.OK {
		fmt.Printf("\nenv %q not yet at %s\n", res.Env, shortSHA(res.Commit))
		return 1
	}
	fmt.Printf("\nenv %q healthy at %s\n", res.Env, shortSHA(res.Commit))
	return 0
}

// shortSHA truncates a commit for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// cmdCheck audits a node's setup. On a node home it runs the node-local checks
// (config, binary, age key, repo checkout, reconcile cron). On a controller
// home it audits every inventory node, re-executing the agent over ssh.
func cmdCheck(g globals, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var update bool
	var controllerVersion string
	fs.BoolVar(&update, "update", false, "controller: push this binary onto any node older than the controller")
	fs.StringVar(&controllerVersion, "controller-version", "", "internal: controller version token for node-side comparison")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] check [--update]")
		fmt.Fprintln(os.Stderr, "  On a node: checks the local setup. On a controller: checks every node.")
		fs.PrintDefaults()
	}
	if _, err := parseFlagsAndArgs(fs, args); err != nil {
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cfg, err := config.Load(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	self := version.Current(buildVersion)

	if cfg.IsController() {
		ok, err := check.Controller(ctx, h, os.Stdout, self, update)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if !ok {
			fmt.Println("\nsome checks failed")
			return 1
		}
		fmt.Println("\nall nodes pass")
		return 0
	}

	ref := version.Info{}
	if controllerVersion != "" {
		ref = version.Parse(controllerVersion)
	}
	results := check.Node(ctx, h, self, ref)
	check.Render(os.Stdout, results)
	if !check.AllOK(results) {
		return 1
	}
	return 0
}

// cmdController handles controller-home administration: initialising the home
// and adding deployment repos to it.
func cmdController(g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] controller <init|repo> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return cmdControllerInit(g, rest)
	case "repo":
		return cmdControllerRepo(g, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown controller subcommand: %s\n", sub)
		return 2
	}
}

func cmdControllerInit(g globals, args []string) int {
	fs := flag.NewFlagSet("controller init", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] controller init")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	log := newLogger(g.jsonLog)
	if err := admin.ControllerInit(h, log); err != nil {
		log.Error("controller init failed", "error", err)
		return 1
	}
	return 0
}

func cmdControllerRepo(g globals, args []string) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] controller repo add <name> <url> [--branch <b>]")
		return 2
	}
	fs := flag.NewFlagSet("controller repo add", flag.ContinueOnError)
	branch := fs.String("branch", "main", "branch to track")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] controller repo add <name> <url> [--branch <b>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args[1:])
	if err != nil {
		return 2
	}
	name, url := arg(pos, 0), arg(pos, 1)
	if name == "" || url == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.ControllerAddRepo(ctx, h, name, url, *branch, log); err != nil {
		log.Error("controller repo add failed", "error", err)
		return 1
	}
	return 0
}

// cmdNodeAdd provisions a new node from the controller: it copies the
// kompensator binary, writes the node's node.yml (following one repo), clones
// that repo on the node and registers it in the repo's inventory.
func cmdNodeAdd(g globals, args []string) int {
	fs := flag.NewFlagSet("node add", flag.ContinueOnError)
	repoName := fs.String("repo", "", "which repo the node follows (default: the sole repo)")
	statusWriteback := fs.Bool("status-writeback", false, "publish reconcile status to the node's git status branch")
	statusWritebackAlways := fs.Bool("status-writeback-always", false, "publish on every reconcile instead of only when the status changed")
	schedule := fs.String("schedule", config.DefaultSchedule, "cron schedule for the node's self-reconcile")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node add <name> <location> [--repo <name>] [--status-writeback] [--status-writeback-always] [--schedule <cron>]")
		fmt.Fprintln(os.Stderr, "  <location> is ssh://[user@]host[:port][/path] (path defaults to ~/.config/kompensator)")
		fmt.Fprintln(os.Stderr, "  or an absolute local path.")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	name, location := arg(pos, 0), arg(pos, 1)
	if name == "" || location == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.ProvisionNode(ctx, admin.ProvisionOptions{
		ControllerHome:        h,
		Name:                  name,
		Location:              location,
		RepoName:              *repoName,
		StatusWriteback:       *statusWriteback,
		StatusWritebackAlways: *statusWritebackAlways,
		Schedule:              *schedule,
		Logger:                log,
	}); err != nil {
		log.Error("node add failed", "error", err)
		return 1
	}
	return 0
}

// cmdNode handles node administration from a controller home: adding a node to
// the inventory and provisioning it, and removing it again.
func cmdNode(g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node <add|remove> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdNodeAdd(g, rest)
	case "remove":
		return cmdNodeRemove(g, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown node subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node <add|remove> ...")
		return 2
	}
}

func cmdNodeRemove(g globals, args []string) int {
	fs := flag.NewFlagSet("node remove", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	keepContainers := fs.Bool("keep-containers", false, "do not tear down the node's containers")
	keepHome := fs.Bool("keep-home", false, "do not delete the node's home directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node remove <name> [--repo <name>] [--keep-containers] [--keep-home]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	name := arg(pos, 0)
	if name == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.NodeRemove(ctx, h, *repoName, name, *keepContainers, *keepHome, log); err != nil {
		log.Error("node remove failed", "error", err)
		return 1
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdSecrets(g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets <set|set-key|set-file|show|edit|rekey> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return cmdSecretsSet(g, rest)
	case "set-key":
		return cmdSecretsSetKey(g, rest)
	case "set-file":
		return cmdSecretsSetFile(g, rest)
	case "show":
		return cmdSecretsShow(g, rest)
	case "edit":
		return cmdSecretsEdit(g, rest)
	case "rekey":
		return cmdSecretsRekey(g, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown secrets subcommand: %s\n", sub)
		return 2
	}
}

func cmdSecretsSet(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets set <env> <stack> [--repo <name>]")
		fmt.Fprintln(os.Stderr, "  Reads a flat YAML map of KEY: value from stdin.")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env, stack := arg(pos, 0), arg(pos, 1)
	if env == "" || stack == "" {
		fs.Usage()
		return 2
	}
	plaintext, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: read stdin:", err)
		return 1
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.SecretsSet(ctx, h, *repoName, env, stack, plaintext, log); err != nil {
		log.Error("secrets set failed", "error", err)
		return 1
	}
	return 0
}

func cmdSecretsSetKey(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets set-key", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets set-key <env> <stack> <KEY> [<value>] [--repo <name>]")
		fmt.Fprintln(os.Stderr, "  Sets a single KEY in the stack's secrets, leaving the others untouched.")
		fmt.Fprintln(os.Stderr, "  With no <value> (or \"-\"), the value is read from stdin.")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env, stack, key := arg(pos, 0), arg(pos, 1), arg(pos, 2)
	if env == "" || stack == "" || key == "" {
		fs.Usage()
		return 2
	}
	value := arg(pos, 3)
	if value == "" || value == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: read stdin:", err)
			return 1
		}
		value = strings.TrimRight(string(raw), "\n")
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.SecretSetKey(ctx, h, *repoName, env, stack, key, value, log); err != nil {
		log.Error("secrets set-key failed", "error", err)
		return 1
	}
	return 0
}

func cmdSecretsSetFile(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets set-file", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets set-file <env> <name> [<source>] [--repo <name>]")
		fmt.Fprintln(os.Stderr, "  Encrypts a file secret's blob for the nodes declared in env.yml.")
		fmt.Fprintln(os.Stderr, "  <source> is @<path> to read a file, or omitted/\"-\" to read stdin.")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env, name := arg(pos, 0), arg(pos, 1)
	if env == "" || name == "" {
		fs.Usage()
		return 2
	}

	var blob []byte
	source := arg(pos, 2)
	if strings.HasPrefix(source, "@") {
		blob, err = os.ReadFile(strings.TrimPrefix(source, "@"))
	} else {
		blob, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: read secret source:", err)
		return 1
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.SecretSetFile(ctx, h, *repoName, env, name, blob, log); err != nil {
		log.Error("secrets set-file failed", "error", err)
		return 1
	}
	return 0
}

func cmdSecretsShow(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets show", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets show <env> <stack> [--repo <name>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env, stack := arg(pos, 0), arg(pos, 1)
	if env == "" || stack == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plaintext, err := admin.SecretsShow(ctx, h, *repoName, env, stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	os.Stdout.Write(plaintext)
	return 0
}

func cmdSecretsEdit(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets edit", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets edit <env> <stack> [--repo <name>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env, stack := arg(pos, 0), arg(pos, 1)
	if env == "" || stack == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.SecretsEdit(ctx, h, *repoName, env, stack, log); err != nil {
		log.Error("secrets edit failed", "error", err)
		return 1
	}
	return 0
}

func cmdSecretsRekey(g globals, args []string) int {
	fs := flag.NewFlagSet("secrets rekey", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets rekey <env> [--repo <name>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	env := arg(pos, 0)
	if env == "" {
		fs.Usage()
		return 2
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if err := admin.SecretsRekey(ctx, h, *repoName, env, log); err != nil {
		log.Error("secrets rekey failed", "error", err)
		return 1
	}
	return 0
}

// parseFlagsAndArgs parses fs allowing flags and positional arguments to appear
// in any order (the stdlib flag package stops parsing at the first positional).
func parseFlagsAndArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// arg returns the i-th element of s or "" if out of range.
func arg(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

func newLogger(jsonLog bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if jsonLog {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func usage() {
	fmt.Fprint(os.Stderr, `kompensator — GitOps deployment for Docker Compose nodes

Usage:
  kompensator [global flags] <command> [args]

Global flags (must come before the command):
  -home string   kompensator home directory (default: $KOMPENSATOR_HOME or ~/.config/kompensator)
  -json          emit structured JSON logs (for journald/Loki)

A kompensator home is either a controller (controller.yml, tracks one or more
repos and drives the nodes) or a node (node.yml, follows one repo and reconciles
itself). The role is detected from which config the home holds.

Commands:
  reconcile [env [stack [project]]]
                    Pull deployment repo(s) and deploy on drift; each argument
                    narrows the run, no env reconciles every environment. A
                    narrowed run neither prunes nor rewrites the env status
                    --force         redeploy even when already in sync
                    --ignore-pause  run despite a pause, without lifting it
                    --repo <name>   limit to one repo (default: all)
  status [env]      Show target vs. running images; no env shows all
                    environments. --repo <name> limits to one repo
  pause             Suspend reconciling on this home, so a delicate operation
                    is not interrupted by a cron tick
                    --reason <text> shown in status and in the reconcile log
                    --timeout <d>   lift automatically after this long
                    --wait <d>      wait for a run already in progress
  resume            Lift a pause
  verify <env>      Check, from git, that every node hosting the env has
                    reconciled the desired commit and is healthy
                    --commit <sha>  desired commit (default: deploy branch tip)
                    --wait          poll until healthy or --timeout
                    --repo-path <d> verify a bare checkout (CI, no home)
  check             Audit a node's setup: on a node checks its config, binary,
                    age key, repo checkout and reconcile cron; on a controller
                    audits every node over ssh
  controller init   Initialise a controller home (writes controller.yml)
  controller repo add <name> <url> [--branch <b>]
                    Add a deployment repo to the controller and clone it
  node add <name> <location>
                    Provision a new node from the controller: copy the binary,
                    write its node.yml (following one repo), clone that repo on
                    it and register it in the repo's inventory. <location> is
                    ssh://[user@]host[:port][/path] or an absolute local path
                    --repo <name>     which repo the node follows
                    --status-writeback / --schedule <cron>
  node remove <name>
                    Deregister a node and tear down its containers and home
                    --keep-containers / --keep-home to skip teardown
                    --repo <name>   which repo's inventory (default: the sole one)
  secrets set <env> <stack>    Encrypt a flat YAML map (read from stdin) of
                               secrets for an environment's stack
  secrets set-key <env> <stack> <KEY> [value]
                               Set a single KEY in a stack's secrets (stdin if
                               no value given)
  secrets set-file <env> <name> [@path]
                               Encrypt a declared file secret's blob (stdin, or
                               @path to read a file) for its target nodes
  secrets show <env> <stack>   Decrypt and print an environment's stack secrets
  secrets edit <env> <stack>   Edit an environment's stack secrets in $EDITOR
  secrets rekey <env>          Re-encrypt an environment's secrets (KV and file)
                               for the current recipient set
  version           Print version
  help              Show this help

Examples:
  kompensator reconcile
  kompensator reconcile dev
  kompensator -json reconcile dev
  kompensator status
  kompensator pause --reason 'db switchover' --timeout 15m --wait 2m
  kompensator resume
  kompensator -home /opt/controller controller init
  kompensator -home /opt/controller controller repo add prod ssh://git@example.org/org/deploy.git
  kompensator -home /opt/controller node add node7 ssh://peter@host.example.org
  kompensator -home /opt/controller node remove node7
  echo 'DB_PASSWORD: s3cr3t' | kompensator -home /opt/controller secrets set prod carimco
  kompensator -home /opt/controller secrets rekey prod
`)
}
