package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

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
	fmt.Fprintln(tw, "NODE\tENV\tAPP\tCOLOR\tCONTAINER\tTARGET\tRUNNING\tHEALTH\tSTATUS")
	for _, s := range statuses {
		state := s.State()
		if state == "drift" || state == "missing" {
			drift++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(s.Node), s.Env, s.App, dash(s.Color), dash(s.Container),
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

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
  version           Print version
  help              Show this help

Examples:
  kompensator reconcile dev
  kompensator -json reconcile dev
  kompensator status
  kompensator -home /opt/kompensator status dev
`)
}
