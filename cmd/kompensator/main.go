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

	"kompensator/internal/admin"
	"kompensator/internal/config"
	"kompensator/internal/reconcile"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

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
	case "node":
		os.Exit(cmdNode(g, rest))
	case "secrets":
		os.Exit(cmdSecrets(g, rest))
	case "version":
		fmt.Println("kompensator", version)
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
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] reconcile [--force] <env>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "  <env> is required.")
		return 2
	}
	env := fs.Arg(0)

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
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] status [env]")
		return 2
	}
	env := "" // empty means: all environments this node participates in
	if len(args) == 1 {
		env = args[0]
	}

	h, err := resolveHome(g.home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	statuses, err := reconcile.Status(ctx, reconcile.Options{Home: h, Env: env})
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
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tENV\tSTACK\tPROJECT\tSERVICE\tCOLOR\tCONTAINER\tTARGET\tRUNNING\tHEALTH\tSTATUS")
	for _, s := range statuses {
		state := s.State()
		if state == "drift" || state == "missing" {
			drift++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(s.Node), s.Env, s.Stack, s.Project, s.Service, dash(s.Color), dash(s.Container),
			dash(s.Desired), dash(s.Running), dash(s.Health), state)
	}
	tw.Flush()

	if drift > 0 {
		fmt.Printf("\n%d container(s) drifting from target.\n", drift)
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

// splitCSV splits a comma-separated list into trimmed, non-empty items.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cmdNodeBootstrap provisions a new node from the controller: it copies the
// kompensator binary, writes the node config (repos copied from the controller
// config), clones the repo(s) on the node and registers it in the inventory.
func cmdNodeBootstrap(g globals, args []string) int {
	fs := flag.NewFlagSet("node bootstrap", flag.ContinueOnError)
	name := fs.String("name", "", "node name in the inventory")
	location := fs.String("location", "", "ssh://[user@]host[:port][/path] (path defaults to ~/.config/kompensator) or an absolute local path")
	envCSV := fs.String("env", "", "comma-separated environments to attach immediately (optional)")
	invRepo := fs.String("repo-inventory", "", "which repo's inventory to register in (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node bootstrap --name <node> --location <loc> [--env e1,e2]")
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	// Allow the name to be given positionally too: node bootstrap <name> --location ...
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
		ControllerHome: h,
		Name:           *name,
		Location:       *location,
		Envs:           splitCSV(*envCSV),
		InventoryRepo:  *invRepo,
		Logger:         log,
	}); err != nil {
		log.Error("node bootstrap failed", "error", err)
		return 1
	}
	return 0
}

func cmdNode(g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: kompensator [global flags] node <bootstrap|add|leave|rm> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "bootstrap":
		return cmdNodeBootstrap(g, rest)
	case "add", "join":
		return cmdNodeSetEnv(g, rest, true)
	case "leave":
		return cmdNodeSetEnv(g, rest, false)
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

func cmdNodeSetEnv(g globals, args []string, join bool) int {
	verb := "leave"
	if join {
		verb = "add"
	}
	fs := flag.NewFlagSet("node "+verb, flag.ContinueOnError)
	repoName := fs.String("repo", "", "deployment repo name (default: the sole repo)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: kompensator [global flags] node %s <name> <env> [--repo <name>]\n", verb)
		fs.PrintDefaults()
	}
	pos, err := parseFlagsAndArgs(fs, args)
	if err != nil {
		return 2
	}
	name, env := arg(pos, 0), arg(pos, 1)
	if name == "" || env == "" {
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
	if err := admin.NodeSetEnv(ctx, h, *repoName, name, env, join, log); err != nil {
		log.Error("node "+verb+" failed", "error", err)
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

Commands:
  reconcile <env>   Pull deployment repo(s) and deploy on drift (env required)
                    --force  redeploy even when already in sync
  status [env]      Show target vs. running images; no env shows all environments
  node bootstrap    Provision a new node from the controller: copy the binary,
                    write its config (repos from the controller), clone the
                    repo(s) on it and register it in the inventory
                    --name <node> --location <loc> [--env e1,e2]
  node add <name> <env>     Attach a node to an environment
  node leave <name> <env>   Detach a node from an environment
  node rm <name>    Deregister a node and tear down its containers and home
                    --keep-containers / --keep-home to skip teardown
  secrets set <env> <stack>    Encrypt a flat YAML map (read from stdin) of
                               secrets for an environment's stack
  secrets show <env> <stack>   Decrypt and print an environment's stack secrets
  secrets edit <env> <stack>   Edit an environment's stack secrets in $EDITOR
  secrets rekey <env>          Re-encrypt an environment's secrets for the
                               current recipient set (run after a node joins)
  version           Print version
  help              Show this help

Examples:
  kompensator reconcile dev
  kompensator -json reconcile dev
  kompensator status
  kompensator -home /opt/controller status dev
  kompensator -home /opt/controller node bootstrap --name node7 --location ssh://peter@host.example.org
  kompensator -home /opt/controller node add node7 dev
  kompensator -home /opt/controller node rm node7
  echo 'DB_PASSWORD: s3cr3t' | kompensator -home /opt/controller secrets set prod carimco
  kompensator -home /opt/controller secrets rekey prod
`)
}
