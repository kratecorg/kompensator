package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Inventory is the set of nodes and their per-environment roles, stored at
// inventory/nodes.yml in the deployment repo.
type Inventory struct {
	Nodes []Node `yaml:"nodes"`
}

// Node is one host in the inventory. A node can participate in multiple
// environments, and within each environment hold multiple roles.
type Node struct {
	Name         string                `yaml:"name"`
	Environments map[string]Membership `yaml:"environments"`
}

// Membership describes a node's participation in one environment.
type Membership struct {
	Roles []string `yaml:"roles"`
}

// RolesFor returns the roles the named node holds in the given environment and
// whether the node participates in that environment at all.
func (inv Inventory) RolesFor(node, env string) ([]string, bool) {
	for _, n := range inv.Nodes {
		if n.Name != node {
			continue
		}
		m, ok := n.Environments[env]
		if !ok {
			return nil, false
		}
		return m.Roles, true
	}
	return nil, false
}

// EnvsForNode returns the sorted list of environments the named node
// participates in, or nil if the node is not in the inventory.
func (inv Inventory) EnvsForNode(node string) []string {
	for _, n := range inv.Nodes {
		if n.Name != node {
			continue
		}
		envs := make([]string, 0, len(n.Environments))
		for env := range n.Environments {
			envs = append(envs, env)
		}
		sort.Strings(envs)
		return envs
	}
	return nil
}

// Placement declares which apps run where, stored at
// environments/<env>/placement.yml.
type Placement struct {
	Apps []App `yaml:"apps"`
}

// App is a deployable unit placed on nodes that hold one of its roles.
type App struct {
	Name string `yaml:"name"`
	// Roles: the app runs on a node if the node holds at least one of these
	// roles in the environment. An empty list matches every node in the env.
	Roles []string `yaml:"roles"`
	// Compose is the compose file path, relative to the environment directory.
	Compose string `yaml:"compose"`
	// Strategy is the deploy strategy. Phase 1 supports "recreate".
	Strategy string `yaml:"strategy"`
}

// DesiredApp is the desired image and tag for an app, from
// environments/<env>/deployment-state.yml.
type DesiredApp struct {
	Image string `yaml:"image"`
	Tag   string `yaml:"tag"`
}

// Ref returns the fully qualified image reference (image:tag).
func (d DesiredApp) Ref() string {
	return d.Image + ":" + d.Tag
}

// LoadInventory reads inventory/nodes.yml from the checked-out repo root.
func LoadInventory(repoRoot string) (Inventory, error) {
	var inv Inventory
	if err := loadYAML(filepath.Join(repoRoot, "inventory", "nodes.yml"), &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// LoadPlacement reads environments/<env>/placement.yml.
func LoadPlacement(repoRoot, env string) (Placement, error) {
	var p Placement
	if err := loadYAML(filepath.Join(repoRoot, "environments", env, "placement.yml"), &p); err != nil {
		return Placement{}, err
	}
	return p, nil
}

// LoadDesiredState reads environments/<env>/deployment-state.yml as a map of
// app name to desired image/tag. Unknown top-level keys (e.g. a future
// metadata block) are ignored.
func LoadDesiredState(repoRoot, env string) (map[string]DesiredApp, error) {
	path := filepath.Join(repoRoot, "environments", env, "deployment-state.yml")
	var state map[string]DesiredApp
	if err := loadYAML(path, &state); err != nil {
		return nil, err
	}
	return state, nil
}

// EnvDir returns the absolute path to an environment directory.
func EnvDir(repoRoot, env string) string {
	return filepath.Join(repoRoot, "environments", env)
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
