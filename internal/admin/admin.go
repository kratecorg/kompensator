// Package admin implements node lifecycle operations: bootstrapping a new node
// (creating its local config and registering it in the deployment repo's
// inventory) and removing one (deregistering it and tearing down its
// containers and home). Inventory changes are committed and pushed to the
// deployment repo origin.
package admin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"kompensator/internal/config"
	"kompensator/internal/gitsync"
	"kompensator/internal/repo"
	"kompensator/internal/runtime"
)

// BootstrapOptions configures creating a new node.
type BootstrapOptions struct {
	Home          string        // the new node's kompensator home
	Name          string        // node name in the inventory
	Location      string        // node location; defaults to the absolute home path
	Repos         []config.Repo // deployment repos the node follows
	Envs          []string      // environments to join
	InventoryRepo string        // which repo's inventory to register in (default: sole repo)
	NoRegister    bool          // only create the local home (config + clone); skip inventory commit+push
	Logger        *slog.Logger
}

// Bootstrap materialises a new node: it writes the node-local config, clones
// the deployment repo(s), and registers the node in the inventory (commit +
// push). It refuses to overwrite an existing config.
func Bootstrap(ctx context.Context, opts BootstrapOptions) error {
	log := logger(opts.Logger)
	if opts.Name == "" {
		return fmt.Errorf("bootstrap requires --name")
	}
	if len(opts.Repos) == 0 {
		return fmt.Errorf("bootstrap requires at least one --repo")
	}

	cfgPath := filepath.Join(opts.Home, "config.yml")
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("config already exists at %s (remove the node first)", cfgPath)
	}

	location := opts.Location
	if location == "" {
		abs, err := filepath.Abs(opts.Home)
		if err != nil {
			return fmt.Errorf("resolve home path: %w", err)
		}
		location = abs
	}

	cfg := config.Config{Node: config.Node{Name: opts.Name}, Repos: opts.Repos}
	// Default branches so the written config is complete.
	for i := range cfg.Repos {
		if cfg.Repos[i].Branch == "" {
			cfg.Repos[i].Branch = "main"
		}
	}
	if err := config.Write(opts.Home, cfg); err != nil {
		return err
	}
	log.Info("wrote node config", "home", opts.Home, "node", opts.Name)

	for _, r := range cfg.Repos {
		dest := filepath.Join(config.ReposDir(opts.Home), r.Name)
		commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
		if err != nil {
			return fmt.Errorf("clone repo %q: %w", r.Name, err)
		}
		log.Info("repo cloned", "repo", r.Name, "commit", commit)
	}

	if opts.NoRegister {
		log.Info("node home ready (not registered in inventory)", "node", opts.Name, "home", opts.Home)
		return nil
	}

	invRepo, err := pickRepo(cfg.Repos, opts.InventoryRepo)
	if err != nil {
		return err
	}
	dest := filepath.Join(config.ReposDir(opts.Home), invRepo.Name)

	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	if err := inv.AddNode(opts.Name, location, opts.Envs); err != nil {
		return err
	}
	if err := repo.SaveInventory(dest, inv); err != nil {
		return err
	}
	if err := gitsync.CommitPush(ctx, dest, invRepo.Branch,
		"inventory: add node "+opts.Name, "inventory/nodes.yml"); err != nil {
		return err
	}

	log.Info("node bootstrapped", "node", opts.Name, "location", location, "envs", opts.Envs)
	return nil
}

// NodeAdd registers an already-existing node in the inventory of a deployment
// repo the controller follows (commit + push). location is required.
func NodeAdd(ctx context.Context, home, repoName, name, location string, envs []string, log *slog.Logger) error {
	log = logger(log)
	if name == "" || location == "" {
		return fmt.Errorf("node add requires a name and --location")
	}
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
	log.Info("node added", "node", name, "location", location, "envs", envs)
	return nil
}

// NodeSetEnv joins or leaves an environment for a node (commit + push).
func NodeSetEnv(ctx context.Context, home, repoName, name, env string, join bool, log *slog.Logger) error {
	log = logger(log)
	if name == "" || env == "" {
		return fmt.Errorf("node join/leave requires a node name and an env")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	action := "leave"
	if join {
		action = "join"
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
	msg := fmt.Sprintf("inventory: node %s %ss env %s", name, action, env)
	if err := gitsync.CommitPush(ctx, dest, r.Branch, msg, "inventory/nodes.yml"); err != nil {
		return err
	}
	log.Info("inventory updated", "action", action, "node", name, "env", env)
	return nil
}

// NodeRemove deregisters a node from the inventory (commit + push) and, unless
// told to keep them, tears down the node's containers and deletes its home.
func NodeRemove(ctx context.Context, home, repoName, name string, keepContainers, keepHome bool, log *slog.Logger) error {
	log = logger(log)
	if name == "" {
		return fmt.Errorf("node rm requires a node name")
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
		projects, err := runtime.ListProjects(ctx, loc.DockerHost(), "kompensator-"+name+"-")
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

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
