package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// File names stored in a kompensator home. A home holds exactly one of them and
// its presence determines the home's role: a controller aggregates many repos
// and drives the nodes; a node follows a single repo and reconciles itself.
const (
	ControllerFile = "controller.yml"
	NodeFile       = "node.yml"
)

const controllerHeader = "# kompensator controller config (managed by 'kompensator controller ...').\n"
const nodeHeader = "# kompensator node-local config (managed by 'kompensator bootstrap').\n"

// Role identifies what a kompensator home is: a controller or a node.
type Role int

const (
	RoleUnknown Role = iota
	RoleController
	RoleNode
)

// Config is the loaded, role-resolved configuration of a kompensator home. It
// never contains secrets.
//
// A controller home (controller.yml) tracks one or more deployment repos and
// has no node identity of its own. A node home (node.yml) has a mandatory node
// name and follows exactly one repo. The two are mutually exclusive: a single
// home is either a controller or a node, never both.
//
// Repos always holds the repos this home acts on: every configured repo for a
// controller, or the single followed repo (a one-element slice) for a node.
type Config struct {
	Role     Role
	NodeName string  // node identity (RoleNode only; empty on a controller)
	Repos    []Repo  // controller: all repos; node: the single followed repo
	Naming   *Naming // optional project-naming overrides

	// StatusWriteback enables publishing this node's reconcile status to git
	// (RoleNode only; default off). The status document is always written
	// locally; this switch only controls whether it is also pushed to the
	// node's status branch.
	StatusWriteback bool
}

// IsController reports whether this home is a controller.
func (c *Config) IsController() bool {
	return c.Role == RoleController
}

// Naming controls which optional leading segments are used in the generated
// Docker Compose project (and therefore container) names. The mandatory
// segments env, stack, project, color, service and replica are always present;
// the deployment repo name and the node name are optional and default to
// enabled. Disable the node segment when every node owns its own host, or the
// repo segment when a single repo makes it redundant.
type Naming struct {
	IncludeRepo *bool `yaml:"includeRepo,omitempty"`
	IncludeNode *bool `yaml:"includeNode,omitempty"`
}

// UseRepo reports whether the deployment repo name is part of project names
// (default true). A nil Naming uses the defaults.
func (n *Naming) UseRepo() bool {
	return n == nil || n.IncludeRepo == nil || *n.IncludeRepo
}

// UseNode reports whether the node name is part of project names (default true).
// A nil Naming uses the defaults.
func (n *Naming) UseNode() bool {
	return n == nil || n.IncludeNode == nil || *n.IncludeNode
}

// Repo is a deployment repository a home tracks.
type Repo struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`
}

// controllerFile is the on-disk shape of controller.yml.
type controllerFile struct {
	Naming *Naming `yaml:"naming,omitempty"`
	Repos  []Repo  `yaml:"repos"`
}

// nodeFile is the on-disk shape of node.yml.
type nodeFile struct {
	Node            string  `yaml:"node"`
	Naming          *Naming `yaml:"naming,omitempty"`
	StatusWriteback bool    `yaml:"statusWriteback,omitempty"`
	Repo            Repo    `yaml:"repo"`
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

// IsOccupied reports whether home already holds a kompensator config (either
// role). Used by bootstrap to avoid clobbering an existing home.
func IsOccupied(home string) bool {
	for _, name := range []string{ControllerFile, NodeFile} {
		if _, err := os.Stat(filepath.Join(home, name)); err == nil {
			return true
		}
	}
	return false
}

// Load reads and validates the config of a kompensator home, resolving its
// role from which config file is present.
func Load(home string) (*Config, error) {
	ctrlPath := filepath.Join(home, ControllerFile)
	nodePath := filepath.Join(home, NodeFile)

	_, ctrlErr := os.Stat(ctrlPath)
	_, nodeErr := os.Stat(nodePath)
	ctrlOK := ctrlErr == nil
	nodeOK := nodeErr == nil

	switch {
	case ctrlOK && nodeOK:
		return nil, fmt.Errorf("home %s has both %s and %s; a home is either a controller or a node", home, ControllerFile, NodeFile)
	case ctrlOK:
		return loadController(ctrlPath)
	case nodeOK:
		return loadNode(nodePath)
	default:
		return nil, fmt.Errorf("no %s or %s in %s: run 'kompensator controller init' or 'kompensator bootstrap'", ControllerFile, NodeFile, home)
	}
}

func loadController(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var f controllerFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	for i := range f.Repos {
		if err := validateRepo(&f.Repos[i], path, i); err != nil {
			return nil, err
		}
	}
	return &Config{Role: RoleController, Repos: f.Repos, Naming: f.Naming}, nil
}

func loadNode(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var f nodeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if f.Node == "" {
		return nil, fmt.Errorf("config %s: node name is required", path)
	}
	if err := validateRepo(&f.Repo, path, 0); err != nil {
		return nil, err
	}
	return &Config{Role: RoleNode, NodeName: f.Node, Repos: []Repo{f.Repo}, Naming: f.Naming, StatusWriteback: f.StatusWriteback}, nil
}

func validateRepo(r *Repo, path string, i int) error {
	if r.Name == "" || r.URL == "" {
		return fmt.Errorf("config %s: repo %d needs name and url", path, i)
	}
	if r.Branch == "" {
		r.Branch = "main"
	}
	return nil
}

// MarshalController renders a controller.yml (header + YAML).
func MarshalController(repos []Repo, naming *Naming) ([]byte, error) {
	return marshal(controllerHeader, controllerFile{Naming: naming, Repos: repos})
}

// MarshalNode renders a node.yml (header + YAML). It is shared by bootstrap when
// provisioning a remote node's config over ssh.
func MarshalNode(nodeName string, r Repo, naming *Naming, statusWriteback bool) ([]byte, error) {
	return marshal(nodeHeader, nodeFile{Node: nodeName, Naming: naming, StatusWriteback: statusWriteback, Repo: r})
}

func marshal(header string, v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	enc.Close()
	return buf.Bytes(), nil
}

// WriteController creates (or overwrites) controller.yml in home.
func WriteController(home string, repos []Repo, naming *Naming) error {
	data, err := MarshalController(repos, naming)
	if err != nil {
		return err
	}
	return writeFile(home, ControllerFile, data)
}

// WriteNode creates (or overwrites) node.yml in home.
func WriteNode(home, nodeName string, r Repo, naming *Naming, statusWriteback bool) error {
	data, err := MarshalNode(nodeName, r, naming, statusWriteback)
	if err != nil {
		return err
	}
	return writeFile(home, NodeFile, data)
}

func writeFile(home, name string, data []byte) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("create home dir: %w", err)
	}
	path := filepath.Join(home, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
