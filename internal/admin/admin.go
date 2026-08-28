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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kratecorg/kompensator/internal/config"
	"github.com/kratecorg/kompensator/internal/cron"
	"github.com/kratecorg/kompensator/internal/gitsync"
	"github.com/kratecorg/kompensator/internal/repo"
	"github.com/kratecorg/kompensator/internal/runtime"
	"github.com/kratecorg/kompensator/internal/secrets"
)

// defaultNodeHome is where a node's kompensator home is placed when an ssh
// location omits the path.
const defaultNodeHome = ".config/kompensator"

// ControllerInit initialises a controller home: it writes an empty
// controller.yml. It refuses to clobber a home that already holds a config.
func ControllerInit(home string, log *slog.Logger) error {
	log = logger(log)
	if config.IsOccupied(home) {
		return fmt.Errorf("home %s already holds a kompensator config", home)
	}
	if err := config.WriteController(home, nil, nil); err != nil {
		return err
	}
	log.Info("controller initialised", "home", home, "config", config.ControllerFile)
	return nil
}

// ControllerAddRepo adds a deployment repo to the controller config and clones
// it into the controller's repos directory.
func ControllerAddRepo(ctx context.Context, home, name, url, branch string, log *slog.Logger) error {
	log = logger(log)
	if name == "" || url == "" {
		return fmt.Errorf("controller repo add requires a name and a url")
	}
	if branch == "" {
		branch = "main"
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	if !cfg.IsController() {
		return fmt.Errorf("controller repo add must run from a controller home (%s)", config.ControllerFile)
	}
	for _, r := range cfg.Repos {
		if r.Name == name {
			return fmt.Errorf("repo %q already configured", name)
		}
	}
	repos := append(append([]config.Repo(nil), cfg.Repos...), config.Repo{Name: name, URL: url, Branch: branch})
	if err := config.WriteController(home, repos, cfg.Naming); err != nil {
		return err
	}
	dest := filepath.Join(config.ReposDir(home), name)
	commit, err := gitsync.SyncInit(ctx, url, branch, dest)
	if err != nil {
		return fmt.Errorf("clone repo %q: %w", name, err)
	}
	// A brand-new deployment repo carries neither the branch nor an inventory.
	// Both are published here, because a node clones the branch during 'node add'
	// before the controller writes the node into the inventory.
	created, err := ensureInventory(ctx, dest, branch)
	if err != nil {
		return fmt.Errorf("repo %q: %w", name, err)
	}
	if created {
		if commit, err = gitsync.Head(ctx, dest); err != nil {
			return fmt.Errorf("repo %q: %w", name, err)
		}
		log.Info("inventory initialised", "repo", name, "file", "inventory/nodes.yml")
	}
	log.Info("repo added", "repo", name, "url", url, "branch", branch, "commit", commit)
	return nil
}

// ensureInventory creates and pushes an empty inventory/nodes.yml when the repo
// has none, so the branch exists on the remote and 'node add' has a file to
// extend. It reports whether it wrote one.
func ensureInventory(ctx context.Context, dest, branch string) (bool, error) {
	if _, err := os.Stat(repo.InventoryPath(dest)); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := repo.SaveInventory(dest, repo.Inventory{}); err != nil {
		return false, err
	}
	if err := gitsync.CommitPush(ctx, dest, branch, "inventory: initialise nodes.yml", "inventory/nodes.yml"); err != nil {
		return false, err
	}
	return true, nil
}

// ProvisionOptions configures provisioning a new node from the controller.
type ProvisionOptions struct {
	ControllerHome  string // the controller's kompensator home (source of repos)
	Name            string // node name in the inventory
	Location        string // ssh://[user@]host[:port][/path] or an absolute local path
	RepoName        string // which repo the node follows (default: the sole repo)
	StatusWriteback bool   // enable publishing reconcile status to git
	// StatusWritebackAlways, when true, publishes on every reconcile; when false
	// (the default) the node only pushes when its status changed in substance.
	StatusWritebackAlways bool
	Schedule              string // cron expression for the node's self-reconcile (default: config.DefaultSchedule)
	Logger                *slog.Logger
}

// ProvisionNode materialises a new node entirely from the controller: it copies
// the kompensator binary, writes the node-local config (the single repo the
// node follows is taken from the controller's config), clones that repo on the
// node and registers the node in the repo's inventory (commit + push). The node
// itself only needs read access to the repo; the controller performs the
// inventory write. After registration it rekeys the repo's environments so the
// new node becomes a recipient of their secrets.
func ProvisionNode(ctx context.Context, opts ProvisionOptions) error {
	log := logger(opts.Logger)
	if opts.Name == "" {
		return fmt.Errorf("node add requires a node name")
	}
	if opts.Location == "" {
		return fmt.Errorf("node add requires a location")
	}

	ctrlCfg, err := config.Load(opts.ControllerHome)
	if err != nil {
		return fmt.Errorf("load controller config: %w", err)
	}
	if !ctrlCfg.IsController() {
		return fmt.Errorf("node add must run from a controller home (%s)", config.ControllerFile)
	}
	r, err := pickRepo(ctrlCfg.Repos, opts.RepoName)
	if err != nil {
		return err
	}

	schedule := opts.Schedule
	if schedule == "" {
		schedule = config.DefaultSchedule
	}

	loc, err := resolveNodeLocation(ctx, opts.Location)
	if err != nil {
		return err
	}
	if err := ensureFreeHome(ctx, loc); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary: %w", err)
	}

	cfgData, err := config.MarshalNode(opts.Name, r, ctrlCfg.Naming, opts.StatusWriteback, opts.StatusWritebackAlways, schedule)
	if err != nil {
		return err
	}

	// Each node gets its own age identity so the controller can encrypt
	// environment secrets for it; the private key never leaves the node.
	privateKey, recipient, err := secrets.GenerateIdentity()
	if err != nil {
		return err
	}

	if loc.Local {
		err = provisionLocal(ctx, log, loc, self, cfgData, privateKey, r, schedule)
	} else {
		err = provisionRemote(ctx, log, loc, self, cfgData, privateKey, r, schedule)
	}
	if err != nil {
		return err
	}
	log.Info("node provisioned", "node", opts.Name, "repo", r.Name, "location", loc.String())

	return registerNode(ctx, log, opts.ControllerHome, r.Name, opts.Name, loc.String(), recipient)
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

// ensureFreeHome rejects a location whose home already holds a kompensator
// config, suggesting the next free sibling home (…/kompensator2, …3, …) so a
// host can run several agents, one per repo, in separate homes.
func ensureFreeHome(ctx context.Context, loc repo.Location) error {
	occupied, err := homeOccupied(ctx, loc)
	if err != nil {
		return err
	}
	if !occupied {
		return nil
	}
	suggestion := suggestFreeHome(ctx, loc)
	return fmt.Errorf("home %s already holds a kompensator config; one home follows one repo. Use a separate home, e.g. --location %s", loc.String(), suggestion)
}

// homeOccupied reports whether the location already holds a controller.yml or
// node.yml.
func homeOccupied(ctx context.Context, loc repo.Location) (bool, error) {
	if loc.Local {
		return config.IsOccupied(loc.Path), nil
	}
	test := fmt.Sprintf("test -e %s || test -e %s",
		shellQuote(loc.Path+"/"+config.ControllerFile),
		shellQuote(loc.Path+"/"+config.NodeFile))
	// remoteRun returns nil when the test command succeeds (a config exists).
	if err := remoteRun(ctx, loc, test); err == nil {
		return true, nil
	}
	return false, nil
}

// suggestFreeHome returns a location whose home is not yet occupied by appending
// an increasing suffix to the home path's last segment. It gives up after a few
// tries and returns the last candidate so the user always gets a hint.
func suggestFreeHome(ctx context.Context, loc repo.Location) string {
	base := strings.TrimRight(loc.Path, "/")
	cand := loc
	for i := 2; i <= 9; i++ {
		cand.Path = fmt.Sprintf("%s%d", base, i)
		occupied, err := homeOccupied(ctx, cand)
		if err == nil && !occupied {
			return cand.String()
		}
	}
	return cand.String()
}

// provisionLocal provisions a node that shares the controller's filesystem.
func provisionLocal(ctx context.Context, log *slog.Logger, loc repo.Location, binary string, cfgData []byte, privateKey string, r config.Repo, schedule string) error {
	if config.IsOccupied(loc.Path) {
		return fmt.Errorf("config already exists at %s (remove the node first)", loc.Path)
	}
	if err := os.MkdirAll(config.ReposDir(loc.Path), 0o755); err != nil {
		return fmt.Errorf("create node home: %w", err)
	}
	if err := copyFile(binary, filepath.Join(loc.Path, "kompensator"), 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	log.Info("copied binary", "dest", filepath.Join(loc.Path, "kompensator"))
	if err := os.WriteFile(filepath.Join(loc.Path, config.NodeFile), cfgData, 0o644); err != nil {
		return fmt.Errorf("write node config: %w", err)
	}
	log.Info("wrote node config", "home", loc.Path)
	if err := secrets.WriteIdentity(secrets.KeyPath(loc.Path), privateKey); err != nil {
		return fmt.Errorf("write age identity: %w", err)
	}
	log.Info("wrote age identity", "home", loc.Path)
	dest := filepath.Join(config.ReposDir(loc.Path), r.Name)
	commit, err := gitsync.Sync(ctx, r.URL, r.Branch, dest)
	if err != nil {
		return fmt.Errorf("clone repo %q: %w", r.Name, err)
	}
	log.Info("repo cloned", "repo", r.Name, "commit", commit)
	if err := cron.InstallLocal(ctx, loc.Path, filepath.Join(loc.Path, "kompensator"), schedule); err != nil {
		return fmt.Errorf("install reconcile cron: %w", err)
	}
	log.Info("reconcile cron installed", "home", loc.Path, "schedule", schedule)
	return nil
}

// provisionRemote provisions a node reachable over ssh.
func provisionRemote(ctx context.Context, log *slog.Logger, loc repo.Location, binary string, cfgData []byte, privateKey string, r config.Repo, schedule string) error {
	reposDir := loc.Path + "/repos"
	if err := remoteRun(ctx, loc, "test ! -e "+shellQuote(loc.Path+"/"+config.NodeFile)); err != nil {
		return fmt.Errorf("config already exists at %s:%s/%s (remove the node first)", loc.Host, loc.Path, config.NodeFile)
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

	if err := remoteWriteFile(ctx, loc, loc.Path+"/"+config.NodeFile, cfgData); err != nil {
		return fmt.Errorf("write node config: %w", err)
	}
	log.Info("wrote node config", "home", loc.Host+":"+loc.Path)

	keyPath := loc.Path + "/" + filepath.Base(secrets.KeyPath(loc.Path))
	if err := remoteWriteFile(ctx, loc, keyPath, []byte(privateKey+"\n")); err != nil {
		return fmt.Errorf("write age identity: %w", err)
	}
	if err := remoteRun(ctx, loc, "chmod 600 "+shellQuote(keyPath)); err != nil {
		return fmt.Errorf("chmod age identity: %w", err)
	}
	log.Info("wrote age identity", "home", loc.Host+":"+loc.Path)

	dest := reposDir + "/" + r.Name
	clone := fmt.Sprintf("git clone -b %s %s %s",
		shellQuote(r.Branch), shellQuote(r.URL), shellQuote(dest))
	if err := remoteRun(ctx, loc, clone); err != nil {
		return fmt.Errorf("clone repo %q on node: %w", r.Name, err)
	}
	log.Info("repo cloned", "repo", r.Name, "dest", dest)
	if err := remoteRun(ctx, loc, cron.InstallScript(loc.Path, loc.Path+"/kompensator", schedule)); err != nil {
		return fmt.Errorf("install reconcile cron on node: %w", err)
	}
	log.Info("reconcile cron installed", "home", loc.Host+":"+loc.Path, "schedule", schedule)
	return nil
}

// registerNode adds the node to the repo's inventory (commit + push) and rekeys
// the repo's environments so the new node becomes a recipient of their secrets.
func registerNode(ctx context.Context, log *slog.Logger, home, repoName, name, location, recipient string) error {
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return err
	}
	if err := inv.AddNode(name, location, recipient); err != nil {
		return err
	}
	if err := repo.SaveInventory(dest, inv); err != nil {
		return err
	}
	if err := gitsync.CommitPush(ctx, dest, r.Branch, "inventory: add node "+name, "inventory/nodes.yml"); err != nil {
		return err
	}
	log.Info("node registered in inventory", "node", name, "location", location)

	// The new node must become a recipient of every environment's secrets, or it
	// cannot decrypt them on reconcile. Rekey each environment to include it.
	envs, err := repo.ListEnvironments(dest)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if err := rekeyEnv(ctx, log, r, dest, home, env); err != nil {
			return fmt.Errorf("rekey %s after adding node: %w", env, err)
		}
	}
	return nil
}

// NodeRemove deregisters a node from the inventory (commit + push) and, unless
// told to keep them, tears down the node's containers and deletes its home.
func NodeRemove(ctx context.Context, home, repoName, name string, keepContainers, keepHome bool, log *slog.Logger) error {
	log = logger(log)
	if name == "" {
		return fmt.Errorf("node remove requires a node name")
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

	// Stop the node self-reconciling: drop its crontab entry. Best-effort, since
	// the node may be unreachable.
	if removed.Location != "" {
		var cronErr error
		if loc.Local {
			cronErr = cron.RemoveLocal(ctx, loc.Path)
		} else {
			cronErr = remoteRun(ctx, loc, cron.RemoveScript(loc.Path))
		}
		if cronErr != nil {
			log.Warn("could not remove reconcile cron on node", "error", cronErr)
		} else {
			log.Info("reconcile cron removed", "node", name)
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

// SecretsSet encrypts the plaintext YAML map of secrets for an environment's
// stack and writes it to environments/<env>/secrets/<stack>.yml.age (commit +
// push). The plaintext is validated as a flat KEY: value YAML map and never
// stored or logged in clear.
func SecretsSet(ctx context.Context, home, repoName, env, stack string, plaintext []byte, log *slog.Logger) error {
	log = logger(log)
	if env == "" || stack == "" {
		return fmt.Errorf("secrets set requires an env and a stack")
	}
	var values map[string]string
	if err := yaml.Unmarshal(plaintext, &values); err != nil {
		return fmt.Errorf("secrets must be a flat YAML map of KEY: value: %w", err)
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	return writeSecrets(ctx, log, r, dest, home, env, stack, values)
}

// SecretsShow decrypts and returns the plaintext secrets for an environment's
// stack, using the controller's own age identity.
func SecretsShow(ctx context.Context, home, repoName, env, stack string) ([]byte, error) {
	if env == "" || stack == "" {
		return nil, fmt.Errorf("secrets show requires an env and a stack")
	}
	_, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(repo.SecretsFile(dest, env, stack))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no secrets for env %q stack %q", env, stack)
		}
		return nil, err
	}
	return secrets.Decrypt(secrets.KeyPath(home), data)
}

// SecretsEdit opens the decrypted secrets for an environment's stack in $EDITOR
// (falling back to vi), then re-encrypts and writes the result. A missing file
// starts from an empty template.
func SecretsEdit(ctx context.Context, home, repoName, env, stack string, log *slog.Logger) error {
	log = logger(log)
	if env == "" || stack == "" {
		return fmt.Errorf("secrets edit requires an env and a stack")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}

	current := []byte("# Secrets for " + env + "/" + stack + " (flat KEY: value map).\n")
	if data, err := os.ReadFile(repo.SecretsFile(dest, env, stack)); err == nil {
		if current, err = secrets.Decrypt(secrets.KeyPath(home), data); err != nil {
			return fmt.Errorf("decrypt existing secrets: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	edited, err := editInEditor(ctx, env+"-"+stack, current)
	if err != nil {
		return err
	}
	var values map[string]string
	if err := yaml.Unmarshal(edited, &values); err != nil {
		return fmt.Errorf("edited secrets are not a valid YAML map: %w", err)
	}
	return writeSecrets(ctx, log, r, dest, home, env, stack, values)
}

// SecretsRekey re-encrypts every secrets file of an environment for the current
// recipient set (controller + the env's nodes). Run it after a node joins an
// environment so the new node can decrypt the environment's secrets.
func SecretsRekey(ctx context.Context, home, repoName, env string, log *slog.Logger) error {
	log = logger(log)
	if env == "" {
		return fmt.Errorf("secrets rekey requires an env")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	return rekeyEnv(ctx, log, r, dest, home, env)
}

// rekeyEnv re-encrypts every *.yml.age of an environment for the current
// recipient set and pushes the change. It is a no-op (no commit) when the
// environment has no secrets. Shared by SecretsRekey and NodeSetEnv.
func rekeyEnv(ctx context.Context, log *slog.Logger, r config.Repo, dest, home, env string) error {
	dir := filepath.Join(dest, "environments", env, "secrets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("no secrets to rekey", "env", env)
			return nil
		}
		return err
	}

	var changed []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml.age") {
			continue
		}
		stack := strings.TrimSuffix(e.Name(), ".yml.age")
		recipients, err := stackRecipients(home, dest, env, stack)
		if err != nil {
			return err
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		values, err := secrets.DecryptMap(secrets.KeyPath(home), data)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", e.Name(), err)
		}
		cipher, err := secrets.EncryptMap(recipients, values)
		if err != nil {
			return err
		}
		if err := os.WriteFile(full, cipher, 0o644); err != nil {
			return err
		}
		changed = append(changed, filepath.Join("environments", env, "secrets", e.Name()))
	}
	fileChanged, err := rekeyEnvFiles(log, dest, home, env)
	if err != nil {
		return err
	}
	changed = append(changed, fileChanged...)

	if len(changed) == 0 {
		log.Info("no secrets to rekey", "env", env)
		return nil
	}
	msg := fmt.Sprintf("secrets: rekey %s", env)
	if err := gitsync.CommitPush(ctx, dest, r.Branch, msg, changed...); err != nil {
		return err
	}
	log.Info("secrets rekeyed", "env", env, "files", len(changed))
	return nil
}

// rekeyEnvFiles re-encrypts every file secret blob of an environment for its
// current recipient set, returning the repo-relative paths it rewrote (to be
// committed by the caller). A blob whose declaration has been removed from
// env.yml is skipped with a warning rather than failing the whole rekey.
func rekeyEnvFiles(log *slog.Logger, dest, home, env string) ([]string, error) {
	dir := repo.SecretsFilesDir(dest, env)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	e, err := repo.LoadEnvironment(dest, env)
	if err != nil {
		return nil, err
	}

	var changed []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".age") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".age")
		declaration, ok := e.Secret(name)
		if !ok {
			log.Warn("file secret blob has no declaration, skipping rekey", "env", env, "secret", name)
			continue
		}
		recipients, err := envSecretRecipients(home, dest, e, declaration)
		if err != nil {
			return nil, err
		}
		full := filepath.Join(dir, entry.Name())
		cipher, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		plaintext, err := secrets.Decrypt(secrets.KeyPath(home), cipher)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", entry.Name(), err)
		}
		reencrypted, err := secrets.Encrypt(recipients, plaintext)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, reencrypted, 0o644); err != nil {
			return nil, err
		}
		changed = append(changed, filepath.Join("environments", env, "secrets", "files", entry.Name()))
	}
	return changed, nil
}

// writeSecrets encrypts values for the env's recipients and writes + pushes the
// secrets file. Shared by SecretsSet and SecretsEdit.
func writeSecrets(ctx context.Context, log *slog.Logger, r config.Repo, dest, home, env, stack string, values map[string]string) error {
	recipients, err := stackRecipients(home, dest, env, stack)
	if err != nil {
		return err
	}
	cipher, err := secrets.EncryptMap(recipients, values)
	if err != nil {
		return err
	}
	rel := filepath.Join("environments", env, "secrets", stack+".yml.age")
	full := filepath.Join(dest, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, cipher, 0o644); err != nil {
		return err
	}
	msg := fmt.Sprintf("secrets: update %s/%s", env, stack)
	if err := gitsync.CommitPush(ctx, dest, r.Branch, msg, rel); err != nil {
		return err
	}
	log.Info("secrets updated", "env", env, "stack", stack, "keys", len(values), "recipients", len(recipients))
	return nil
}

// SecretSetKey sets (or adds) a single KEY: value entry in an environment's
// stack secrets, leaving every other key untouched. It loads and decrypts the
// existing map (an absent file starts empty), replaces the one key, and
// re-encrypts + pushes for the stack's current recipients.
func SecretSetKey(ctx context.Context, home, repoName, env, stack, key, value string, log *slog.Logger) error {
	log = logger(log)
	if env == "" || stack == "" || key == "" {
		return fmt.Errorf("secrets set-key requires an env, a stack and a key")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	values := map[string]string{}
	if data, err := os.ReadFile(repo.SecretsFile(dest, env, stack)); err == nil {
		if values, err = secrets.DecryptMap(secrets.KeyPath(home), data); err != nil {
			return fmt.Errorf("decrypt existing secrets: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if values == nil {
		values = map[string]string{}
	}
	values[key] = value
	return writeSecrets(ctx, log, r, dest, home, env, stack, values)
}

// SecretSetFile encrypts a file secret's blob for its declared recipients and
// writes it to environments/<env>/secrets/files/<name>.age (commit + push). The
// secret must already be declared in env.yml; writing an undeclared blob is
// rejected so an orphan file (that no node would ever materialise) never lands
// in the repo.
func SecretSetFile(ctx context.Context, home, repoName, env, name string, blob []byte, log *slog.Logger) error {
	log = logger(log)
	if env == "" || name == "" {
		return fmt.Errorf("secrets set-file requires an env and a secret name")
	}
	r, dest, err := syncedRepo(ctx, home, repoName)
	if err != nil {
		return err
	}
	e, err := repo.LoadEnvironment(dest, env)
	if err != nil {
		return err
	}
	declaration, ok := e.Secret(name)
	if !ok {
		return fmt.Errorf("env %q declares no secret %q; add it to env.yml first", env, name)
	}
	if err := declaration.Validate(); err != nil {
		return err
	}
	recipients, err := envSecretRecipients(home, dest, e, declaration)
	if err != nil {
		return err
	}
	cipher, err := secrets.Encrypt(recipients, blob)
	if err != nil {
		return err
	}
	rel := filepath.Join("environments", env, "secrets", "files", name+".age")
	full := repo.SecretFileBlob(dest, env, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, cipher, 0o644); err != nil {
		return err
	}
	msg := fmt.Sprintf("secrets: update file %s/%s", env, name)
	if err := gitsync.CommitPush(ctx, dest, r.Branch, msg, rel); err != nil {
		return err
	}
	log.Info("file secret updated", "env", env, "secret", name, "bytes", len(blob), "recipients", len(recipients))
	return nil
}

// envSecretRecipients returns the age recipients a file secret is encrypted
// for: the controller's own identity (so it can rekey later) plus the nodes the
// secret targets. An unpinned secret uses every node of the environment; a
// pinned one only the nodes in its list.
func envSecretRecipients(home, dest string, e repo.Environment, s repo.EnvSecret) ([]string, error) {
	controllerRecipient, created, err := secrets.LoadOrCreateIdentity(home)
	if err != nil {
		return nil, err
	}
	if created {
		slog.Default().Info("created controller age identity", "recipient", controllerRecipient)
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return nil, err
	}
	var nodes []string
	if len(s.Nodes) == 0 {
		nodes = e.ParticipatingNodes(inv.AllNodeNames())
	} else {
		nodes = []string(s.Nodes)
	}
	recipients := append([]string{controllerRecipient}, inv.RecipientsForNodes(nodes)...)
	return dedupeStrings(recipients), nil
}

// stackRecipients returns the age recipients a stack's secrets are encrypted
// for: the controller's own identity (so it can keep editing) plus the nodes
// that actually run the stack. An unpinned stack uses every node in the
// environment; a stack (or its projects) pinned to a node subset uses only
// those nodes, so a pinned stack's secret (e.g. a database password) is
// decryptable solely by the node(s) that run it. The controller identity is
// created on first use.
func stackRecipients(home, dest, env, stack string) ([]string, error) {
	controllerRecipient, created, err := secrets.LoadOrCreateIdentity(home)
	if err != nil {
		return nil, err
	}
	if created {
		slog.Default().Info("created controller age identity", "recipient", controllerRecipient)
	}
	inv, err := repo.LoadInventory(dest)
	if err != nil {
		return nil, err
	}
	e, err := repo.LoadEnvironment(dest, env)
	if err != nil {
		return nil, err
	}
	// Project names let NodesRunningStack honor project-level pins; if the stack
	// definition is unavailable, fall back to stack-level placement only.
	var projectNames []string
	if s, err := repo.LoadStack(dest, stack); err == nil {
		for _, p := range s.Projects {
			projectNames = append(projectNames, p.Name)
		}
	}
	runningNodes := e.NodesRunningStack(stack, projectNames, inv.AllNodeNames())
	recipients := append([]string{controllerRecipient}, inv.RecipientsForNodes(runningNodes)...)
	return dedupeStrings(recipients), nil
}

// editInEditor writes content to a temp file, opens it in $EDITOR (or vi), and
// returns the edited bytes.
func editInEditor(ctx context.Context, name string, content []byte) ([]byte, error) {
	f, err := os.CreateTemp("", "kompensator-secret-"+name+"-*.yml")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(content); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.CommandContext(ctx, editor, tmp)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("editor %q: %w", editor, err)
	}
	return os.ReadFile(tmp)
}

// dedupeStrings returns s without duplicates, preserving first-seen order.
func dedupeStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := s[:0]
	for _, v := range s {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
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
