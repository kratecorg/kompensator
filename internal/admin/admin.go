// Package admin implements node lifecycle operations driven from a controller:
// provisioning a new node (copying the kompensator binary, writing its config,
// cloning the deployment repo(s) on it and registering it in the inventory),
// attaching it to / detaching it from environments, and removing it (tearing
// down its containers and home). Inventory changes are committed and pushed to
// the deployment repo origin from the controller, which holds write access.
package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"kompensator/internal/config"
	"kompensator/internal/gitsync"
	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// defaultNodeHome is where a node's kompensator home is placed when an ssh
// location omits the path.
const defaultNodeHome = ".config/kompensator"

// ProvisionOptions configures provisioning a new node from the controller.
type ProvisionOptions struct {
	ControllerHome string   // the controller's kompensator home (source of repos)
	Name           string   // node name in the inventory
	Location       string   // ssh://[user@]host[:port][/path] or an absolute local path
	Envs           []string // optional environments to attach immediately
	InventoryRepo  string   // which repo's inventory to register in (default: sole repo)
	Logger         *slog.Logger
}

// ProvisionNode materialises a new node entirely from the controller: it copies
// the kompensator binary, writes the node-local config (the repos are taken
// from the controller's config), clones the deployment repo(s) on the node and
// registers the node in the inventory (commit + push). The node itself only
// needs read access to the repos; the controller performs the inventory write.
func ProvisionNode(ctx context.Context, opts ProvisionOptions) error {
	log := logger(opts.Logger)
	if opts.Name == "" {
		return fmt.Errorf("node bootstrap requires --name")
	}
	if opts.Location == "" {
		return fmt.Errorf("node bootstrap requires --location")
	}

	ctrlCfg, err := config.Load(opts.ControllerHome)
	if err != nil {
		return fmt.Errorf("load controller config: %w", err)
	}
	if len(ctrlCfg.Repos) == 0 {
		return fmt.Errorf("controller config has no repos to copy to the node")
	}

	loc, err := resolveNodeLocation(ctx, opts.Location)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary: %w", err)
	}

	nodeCfg := config.Config{Node: config.Node{Name: opts.Name}, Naming: ctrlCfg.Naming, Repos: ctrlCfg.Repos}
	cfgData, err := config.Marshal(nodeCfg)
	if err != nil {
		return err
	}

	if loc.Local {
		err = provisionLocal(ctx, log, loc, self, cfgData, nodeCfg.Repos)
	} else {
		err = provisionRemote(ctx, log, loc, self, cfgData, nodeCfg.Repos)
	}
	if err != nil {
		return err
	}
	log.Info("node provisioned", "node", opts.Name, "location", loc.String())

	return registerNode(ctx, log, opts.ControllerHome, opts.InventoryRepo, opts.Name, loc.String(), opts.Envs)
}

// resolveNodeLocation parses a provisioning location. For an ssh location with
// no path, it resolves the remote $HOME and defaults to ~/.config/kompensator
// so the inventory always stores an absolute path.
func resolveNodeLocation(ctx context.Context, raw string) (repo.Location, error) {
	loc, err := repo.ParseLocation(raw)
	if err != nil {
		return repo.Location{}, err
	}
	if loc.Local {
		return loc, nil
	}
	if loc.Path == "" || loc.Path == "/" {
		home, err := remoteHome(ctx, loc)
		if err != nil {
			return repo.Location{}, err
		}
		loc.Path = home + "/" + defaultNodeHome
	}
	return loc, nil
}

// provisionLocal provisions a node that shares the controller's filesystem.
func provisionLocal(ctx context.Context, log *slog.Logger, loc repo.Location, binary string, cfgData []byte, repos []config.Repo) error {
	if _, err := os.Stat(filepath.Join(loc.Path, "config.yml")); err == nil {
		return fmt.Errorf("config already exists at %s (remove the node first)", loc.Path)
	}
	if err := os.MkdirAll(config.ReposDir(loc.Path), 0o755); err != nil {
		return fmt.Errorf("create node home: %w", err)
	}
	if err := copyFile(binary, filepath.Join(loc.Path, "kompensator"), 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	log.Info("copied binary", "dest", filepath.Join(loc.Path, "kompensator"))
	if err := os.WriteFile(filepath.Join(loc.Path, "config.yml"), cfgData, 0o644); err != nil {
		return fmt.Errorf("write node config: %w", err)
	}
	log.Info("wrote node config", "home", loc.Path)
	for _, r := range repos {
		dest := filepath.Join(config.ReposDir(loc.Path), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return fmt.Errorf("clone repo %q: %w", r.Name, err)
		}
		log.Info("repo cloned", "repo", r.Name, "commit", commit)
	}
	return nil
}

// provisionRemote provisions a node reachable over ssh.
func provisionRemote(ctx context.Context, log *slog.Logger, loc repo.Location, binary string, cfgData []byte, repos []config.Repo) error {
	reposDir := loc.Path + "/repos"
	if err := remoteRun(ctx, loc, "test ! -e "+shellQuote(loc.Path+"/config.yml")); err != nil {
		return fmt.Errorf("config already exists at %s:%s/config.yml (remove the node first)", loc.Host, loc.Path)
	}
	if err := remoteRun(ctx, loc, "mkdir -p "+shellQuote(reposDir)); err != nil {
		return fmt.Errorf("create node home: %w", err)
	}

	binaryPath := loc.Path + "/kompensator"
	if err := scpFile(ctx, loc, binary, binaryPath); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := remoteRun(ctx, loc, "chmod +x "+shellQuote(binaryPath)); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}
	log.Info("copied binary", "dest", loc.Host+":"+binaryPath)

	if err := remoteWriteFile(ctx, loc, loc.Path+"/config.yml", cfgData); err != nil {
		return fmt.Errorf("write node config: %w", err)
	}
	log.Info("wrote node config", "home", loc.Host+":"+loc.Path)

	for _, r := range repos {
		dest := reposDir + "/" + r.Name
		clone := fmt.Sprintf("git clone -b %s %s %s",
			shellQuote(r.Branch), shellQuote(r.URL), shellQuote(dest))
		if err := remoteRun(ctx, loc, clone); err != nil {
			return fmt.Errorf("clone repo %q on node: %w", r.Name, err)
		}
		log.Info("repo cloned", "repo", r.Name, "dest", dest)
	}
	return nil
}

// registerNode adds the node to the inventory of a deployment repo the
// controller follows and commits + pushes the change.
func registerNode(ctx context.Context, log *slog.Logger, home, repoName, name, location string, envs []string) error {
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	if err := inv.AddNode(name, location, envs); err != nil {
		return err
	}
	if err := repo.SaveInventory(dest, inv); err != nil {
		return err
	}
	if err := gitsync.CommitPush(ctx, dest, r.Branch, "inventory: add node "+name, "inventory/nodes.yml"); err != nil {
		return err
	}
	log.Info("node registered in inventory", "node", name, "location", location, "envs", envs)
	return nil
}

// NodeSetEnv joins or leaves an environment for a node (commit + push).
func NodeSetEnv(ctx context.Context, home, repoName, name, env string, join bool, log *slog.Logger) error {
	log = logger(log)
	if name == "" || env == "" {
		return fmt.Errorf("node add/leave requires a node name and an env")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	action := "left"
	if join {
		action = "added to"
		err = inv.JoinEnv(name, env)
	} else {
		err = inv.LeaveEnv(name, env)
	}
	if err != nil {
		return err
	}
	if err := repo.SaveInventory(dest, inv); err != nil {
		return err
	}
	msg := fmt.Sprintf("inventory: node %s %s env %s", name, action, env)
	if err := gitsync.CommitPush(ctx, dest, r.Branch, msg, "inventory/nodes.yml"); err != nil {
		return err
	}
	log.Info("inventory updated", "node", name, "action", action, "env", env)
	return nil
}

// NodeRemove deregisters a node from the inventory (commit + push) and, unless
// told to keep them, tears down the node's containers and deletes its home.
func NodeRemove(ctx context.Context, home, repoName, name string, keepContainers, keepHome bool, log *slog.Logger) error {
	log = logger(log)
	if name == "" {
		return fmt.Errorf("node rm requires a node name")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	removed, err := inv.RemoveNode(name)
	if err != nil {
		return err
	}
	if err := repo.SaveInventory(dest, inv); err != nil {
		return err
	}
	if err := gitsync.CommitPush(ctx, dest, r.Branch, "inventory: remove node "+name, "inventory/nodes.yml"); err != nil {
		return err
	}
	log.Info("node deregistered", "node", name)

	// Resolve the node's location for teardown. A missing/invalid location is
	// not fatal: we just skip what we cannot reach.
	var loc repo.Location
	if removed.Location != "" {
		if loc, err = repo.ParseLocation(removed.Location); err != nil {
			log.Warn("cannot parse node location, skipping container/home teardown", "location", removed.Location, "error", err)
			return nil
		}
	}

	if !keepContainers {
		names := runtime.Names{
			Repo: r.Name, Node: name,
			IncludeRepo: cfg.Naming.UseRepo(), IncludeNode: cfg.Naming.UseNode(),
		}
		prefix := names.TeardownPrefix()
		if prefix == "" {
			log.Warn("naming includes neither repo nor node; cannot scope container teardown to this node, skipping", "node", name)
		} else {
			projects, err := runtime.ListProjects(ctx, loc.DockerHost(), prefix)
			if err != nil {
				return fmt.Errorf("list node projects: %w", err)
			}
			for _, p := range projects {
				log.Info("tearing down project", "project", p)
				if err := runtime.Down(ctx, loc.DockerHost(), p); err != nil {
					return err
				}
			}
		}
	}

	if !keepHome {
		if loc.Local && loc.Path != "" {
			log.Info("removing node home", "path", loc.Path)
			if err := os.RemoveAll(loc.Path); err != nil {
				return fmt.Errorf("remove node home %s: %w", loc.Path, err)
			}
		} else if !loc.Local {
			log.Warn("node is remote, leaving its home in place", "host", loc.Host, "path", loc.Path)
		}
	}

	log.Info("node removed", "node", name)
	return nil
}

// syncedRepo loads the home config, picks the deployment repo (by name or the
// sole one) and pulls it so inventory edits start from the latest state.
func syncedRepo(ctx context.Context, home, repoName string) (config.Repo, string, error) {
	cfg, err := config.Load(home)
	if err != nil {
		return config.Repo{}, "", err
	}
	r, err := pickRepo(cfg.Repos, repoName)
	if err != nil {
		return config.Repo{}, "", err
	}
	dest := filepath.Join(config.ReposDir(home), r.Name)
	if _, err := gitsync.Sync(ctx, r.URL, r.Branch, dest); err != nil {
		return config.Repo{}, "", fmt.Errorf("sync repo %q: %w", r.Name, err)
	}
	return r, dest, nil
}

// pickRepo selects a deployment repo by name, or the sole one when name is
// empty. It errors when the choice is ambiguous or the name is unknown.
func pickRepo(repos []config.Repo, name string) (config.Repo, error) {
	if name != "" {
		for _, r := range repos {
			if r.Name == name {
				return r, nil
			}
		}
		return config.Repo{}, fmt.Errorf("repo %q not configured", name)
	}
	if len(repos) == 1 {
		return repos[0], nil
	}
	return config.Repo{}, fmt.Errorf("multiple repos configured; specify --repo")
}

// sshTarget returns the "[user@]host" part for ssh/scp.
func sshTarget(loc repo.Location) string {
	if loc.User != "" {
		return loc.User + "@" + loc.Host
	}
	return loc.Host
}

// sshArgs returns the common ssh options (batch mode, optional port).
func sshArgs(loc repo.Location) []string {
	args := []string{"-o", "BatchMode=yes"}
	if loc.Port != "" {
		args = append(args, "-p", loc.Port)
	}
	return args
}

// remoteHome resolves $HOME on the node over ssh.
func remoteHome(ctx context.Context, loc repo.Location) (string, error) {
	args := append(sshArgs(loc), sshTarget(loc), `printf %s "$HOME"`)
	out, err := exec.CommandContext(ctx, "ssh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("resolve remote home on %s: %w", loc.Host, err)
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", fmt.Errorf("remote home on %s is empty", loc.Host)
	}
	return home, nil
}

// remoteRun runs a shell command on the node over ssh, streaming its output to
// the controller's stderr.
func remoteRun(ctx context.Context, loc repo.Location, command string) error {
	args := append(sshArgs(loc), sshTarget(loc), command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// remoteWriteFile writes data to a file on the node over ssh.
func remoteWriteFile(ctx context.Context, loc repo.Location, path string, data []byte) error {
	args := append(sshArgs(loc), sshTarget(loc), "cat > "+shellQuote(path))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// scpFile copies a local file to destPath on the node.
func scpFile(ctx context.Context, loc repo.Location, src, destPath string) error {
	args := []string{"-o", "BatchMode=yes"}
	if loc.Port != "" {
		// scp uses an upper-case -P for the port.
		args = append(args, "-P", loc.Port)
	}
	args = append(args, src, sshTarget(loc)+":"+destPath)
	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// copyFile copies src to dst with the given mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// shellQuote single-quotes a string for safe use in a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
