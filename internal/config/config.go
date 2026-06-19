package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the node-local configuration stored under the kompensator home
// directory (config.yml). It identifies this node and the deployment repos it
// follows. It never contains secrets.
type Config struct {
	Node  Node   `yaml:"node"`
	Repos []Repo `yaml:"repos"`
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

	if cfg.Node.Name == "" {
		return nil, fmt.Errorf("config %s: node.name is required", path)
	}
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
