package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configHeader is prepended to a generated config.yml.
const configHeader = "# kompensator node-local config (managed by 'kompensator bootstrap').\n"

// Config is the node-local configuration stored under the kompensator home
// directory (config.yml). It identifies this node and the deployment repos it
// follows. It never contains secrets.
//
// node.name is optional: a host that only acts as a controller (aggregating
// the status of all nodes from a checked-out deployment repo) leaves it empty.
// A reconcile agent, by contrast, requires node.name.
type Config struct {
	Node  Node   `yaml:"node"`
	Repos []Repo `yaml:"repos"`
	// Agent is the kompensator binary path a controller invokes on remote nodes
	// over ssh (e.g. "/home/peter/.local/bin/kompensator" when it is not on the
	// non-interactive ssh PATH). Empty defaults to "kompensator".
	Agent string `yaml:"agent"`
}

// IsController reports whether this config is for a controller-only host, i.e.
// one that has no node identity of its own and only aggregates node status.
func (c *Config) IsController() bool {
	return c.Node.Name == ""
}

// Node identifies this machine within a deployment repo's inventory.
type Node struct {
	Name string `yaml:"name"`
}

// Repo is a deployment repository the node tracks.
type Repo struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
}

// Home returns the kompensator home directory.
//
// Resolution order:
//  1. $KOMPENSATOR_HOME
//  2. $XDG_CONFIG_HOME/kompensator
//  3. $HOME/.config/kompensator
func Home() (string, error) {
	if h := os.Getenv("KOMPENSATOR_HOME"); h != "" {
		return h, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "kompensator"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".config", "kompensator"), nil
}

// ReposDir returns the directory where deployment repos are checked out.
func ReposDir(home string) string {
	return filepath.Join(home, "repos")
}

// Load reads and validates config.yml from the given home directory.
func Load(home string) (*Config, error) {
	path := filepath.Join(home, "config.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// node.name is optional (a controller-only host has none). When present, a
	// reconcile run uses it; Run enforces that it is set.
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("config %s: at least one repo is required", path)
	}
	for i := range cfg.Repos {
		r := &cfg.Repos[i]
		if r.Name == "" || r.URL == "" {
			return nil, fmt.Errorf("config %s: repo %d needs name and url", path, i)
		}
		if r.Branch == "" {
			r.Branch = "main"
		}
	}

	return &cfg, nil
}

// Write creates (or overwrites) config.yml in home from cfg. It is used by
// bootstrap to materialise a new node's local configuration.
func Write(home string, cfg Config) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("create home dir: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(configHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	enc.Close()

	path := filepath.Join(home, "config.yml")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
