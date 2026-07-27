package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"kompensator/internal/admin"
	"kompensator/internal/check"
	"kompensator/internal/config"
	"kompensator/internal/reconcile"
	"kompensator/internal/verify"
	"kompensator/internal/version"
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
	case "status":
		os.Exit(cmdStatus(g, rest))
	case "verify":
		os.Exit(cmdVerify(g, rest))
	case "check":
		os.Exit(cmdCheck(g, rest))
	case "controller":
		os.Exit(cmdController(g, rest))
	case "bootstrap":
		os.Exit(cmdBootstrap(g, rest))
	case "node":
		os.Exit(cmdNode(g, rest))
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
	repoName := fs.String("repo", "", "limit to this repo (default: all configured repos)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] reconcile [--force] [--repo <name>] [<env>]")
		fmt.Fprintln(os.Stderr, "  Omitting <env> reconciles every environment.")
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
	env := arg(pos, 0)

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := newLogger(g.jsonLog)
	if _, err := reconcile.Run(ctx, reconcile.Options{
		Home:    h,
		Env:     env,
		Repo:    *repoName,
		Force:   *force,
		JSONLog: g.jsonLog,
		Logger:  log,
	}); err != nil {
		log.Error("reconcile failed", "error", err)
		return 1
	}
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

// cmdCheck audits a bootstrap. On a node home it runs the node-local checks
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
		fmt.Fprintln(os.Stderr, "  On a node: checks the local bootstrap. On a controller: checks every node.")
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

// cmdBootstrap provisions a new node from the controller: it copies the
// kompensator binary, writes the node's node.yml (following one repo), clones
// that repo on the node and registers it in the repo's inventory.
func cmdBootstrap(g globals, args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	name := fs.String("name", "", "node name in the inventory")
	location := fs.String("location", "", "ssh://[user@]host[:port][/path] (path defaults to ~/.config/kompensator) or an absolute local path")
	repoName := fs.String("repo", "", "which repo the node follows (default: the sole repo)")
	statusWriteback := fs.Bool("status-writeback", false, "publish reconcile status to the node's git status branch")
	statusWritebackAlways := fs.Bool("status-writeback-always", false, "publish on every reconcile instead of only when the status changed")
	schedule := fs.String("schedule", config.DefaultSchedule, "cron schedule for the node's self-reconcile")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] bootstrap --name <node> --location <loc> [--repo <name>] [--status-writeback] [--status-writeback-always] [--schedule <cron>]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	// Allow the name to be given positionally too: bootstrap <name> --location ...
	if *name == "" {
		*name = arg(pos, 0)
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
		Name:                  *name,
		Location:              *location,
		RepoName:              *repoName,
		StatusWriteback:       *statusWriteback,
		StatusWritebackAlways: *statusWritebackAlways,
		Schedule:              *schedule,
		Logger:                log,
	}); err != nil {
		log.Error("bootstrap failed", "error", err)
		return 1
	}
	return 0
}

func cmdNode(g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node <rm> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "rm", "remove":
		return cmdNodeRemove(g, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown node subcommand: %s\n", sub)
		return 2
	}
}

func cmdNodeRemove(g globals, args []string) int {
	fs := flag.NewFlagSet("node rm", flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	keepContainers := fs.Bool("keep-containers", false, "do not tear down the node's containers")
	keepHome := fs.Bool("keep-home", false, "do not delete the node's home directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node rm <name> [--repo <name>] [--keep-containers] [--keep-home]")
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
		log.Error("node rm failed", "error", err)
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
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] secrets <set|show|edit|rekey> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return cmdSecretsSet(g, rest)
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
  reconcile [env]   Pull deployment repo(s) and deploy on drift; no env
                    reconciles every environment
                    --force         redeploy even when already in sync
                    --repo <name>   limit to one repo (default: all)
  status [env]      Show target vs. running images; no env shows all
                    environments. --repo <name> limits to one repo
  verify <env>      Check, from git, that every node hosting the env has
                    reconciled the desired commit and is healthy
                    --commit <sha>  desired commit (default: deploy branch tip)
                    --wait          poll until healthy or --timeout
                    --repo-path <d> verify a bare checkout (CI, no home)
  check             Audit a bootstrap: on a node checks its config, binary,
                    age key, repo checkout and reconcile cron; on a controller
                    audits every node over ssh
  controller init   Initialise a controller home (writes controller.yml)
  controller repo add <name> <url> [--branch <b>]
                    Add a deployment repo to the controller and clone it
  bootstrap         Provision a new node from the controller: copy the binary,
                    write its node.yml (following one repo), clone that repo on
                    it and register it in the repo's inventory
                    --name <node> --location <loc> [--repo <name>]
                    [--status-writeback] [--schedule <cron>]
  node rm <name>    Deregister a node and tear down its containers and home
                    --keep-containers / --keep-home to skip teardown
                    --repo <name>   which repo's inventory (default: the sole one)
  secrets set <env> <stack>    Encrypt a flat YAML map (read from stdin) of
                               secrets for an environment's stack
  secrets show <env> <stack>   Decrypt and print an environment's stack secrets
  secrets edit <env> <stack>   Edit an environment's stack secrets in $EDITOR
  secrets rekey <env>          Re-encrypt an environment's secrets for the
                               current recipient set
  version           Print version
  help              Show this help

Examples:
  kompensator reconcile
  kompensator reconcile dev
  kompensator -json reconcile dev
  kompensator status
  kompensator -home /opt/controller controller init
  kompensator -home /opt/controller controller repo add prod ssh://git@example.org/org/deploy.git
  kompensator -home /opt/controller bootstrap --name node7 --location ssh://peter@host.example.org
  kompensator -home /opt/controller node rm node7
  echo 'DB_PASSWORD: s3cr3t' | kompensator -home /opt/controller secrets set prod carimco
  kompensator -home /opt/controller secrets rekey prod
`)
}
