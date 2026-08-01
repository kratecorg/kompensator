package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// statusDoc is the node-local reconcile status for one environment, persisted
// to <home>/status/<env>.yml.
//
// It is a living document with two kinds of content:
//
//   - Per-project config fingerprints (projectStatus.ConfigHash), updated
//     incrementally as each project deploys. These replace the old
//     <home>/deploy-state/<project> files and drive drift detection: a project
//     whose fingerprint still matches is in sync and is skipped.
//   - Run-level observation (DesiredCommit, ReconciledAt, Healthy) plus the
//     per-service running state, finalised once at the end of a node reconcile.
//
// The document is always written locally. When the node enables status
// write-back it is also published to git for a CI pipeline to observe, keyed on
// DesiredCommit (the reconciled repo commit) and Healthy.
type statusDoc struct {
	Node          string                    `yaml:"node,omitempty"`
	Env           string                    `yaml:"env,omitempty"`
	DesiredCommit string                    `yaml:"desiredCommit,omitempty"`
	ReconciledAt  string                    `yaml:"reconciledAt,omitempty"`
	Healthy       bool                      `yaml:"healthy"`
	Projects      map[string]*projectStatus `yaml:"projects,omitempty"`
	// Secrets records the content hash of each file secret last materialised on
	// this node, keyed by secret name. It drives change detection: a secret whose
	// hash still matches is already on disk and its reload hook is not re-run.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}

// projectStatus is the recorded state of one project, keyed in the document by
// "<stack>/<project>".
type projectStatus struct {
	ConfigHash string                  `yaml:"configHash,omitempty"`
	Services   map[string]serviceState `yaml:"services,omitempty"`
}

// serviceState is the observed running state of one service within a project.
type serviceState struct {
	Tag    string `yaml:"tag,omitempty"`
	Color  string `yaml:"color,omitempty"`
	Health string `yaml:"health,omitempty"`
	InSync bool   `yaml:"inSync"`
}

// statusDir is where per-environment status documents live on a node.
func statusDir(home string) string {
	return filepath.Join(home, "status")
}

// statusFilePath is the on-disk location of an environment's status document.
func statusFilePath(home, env string) string {
	return filepath.Join(statusDir(home), env+".yml")
}

// loadStatusDoc reads the status document for an environment, returning an empty
// document (not an error) when none exists yet.
func loadStatusDoc(home, env string) (*statusDoc, error) {
	data, err := os.ReadFile(statusFilePath(home, env))
	if err != nil {
		if os.IsNotExist(err) {
			return &statusDoc{Projects: map[string]*projectStatus{}}, nil
		}
		return nil, err
	}
	var d statusDoc
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse status %s: %w", statusFilePath(home, env), err)
	}
	if d.Projects == nil {
		d.Projects = map[string]*projectStatus{}
	}
	return &d, nil
}

// save writes the status document atomically (temp file + rename) so a crashed
// reconcile never leaves a half-written document behind.
func (d *statusDoc) save(home, env string) error {
	dir := statusDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf []byte
	out, err := yaml.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	buf = out
	tmp, err := os.CreateTemp(dir, env+".yml.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, statusFilePath(home, env))
}

// project returns the project entry for key, creating it when absent.
func (d *statusDoc) project(key string) *projectStatus {
	if d.Projects == nil {
		d.Projects = map[string]*projectStatus{}
	}
	p := d.Projects[key]
	if p == nil {
		p = &projectStatus{}
		d.Projects[key] = p
	}
	return p
}

// projectKeys returns the document's project keys, sorted.
func (d *statusDoc) projectKeys() []string {
	keys := make([]string, 0, len(d.Projects))
	for k := range d.Projects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
