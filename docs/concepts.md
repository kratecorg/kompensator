# Concepts & Glossary

## Core concepts

### Desired state
The target configuration declared in Git: for each app, which image and tag should
run. Updated by humans (promotions) or by CI (new builds). This is the single source
of truth.

### Actual state
What is really running on a node right now — the image tags of the live Docker
Compose containers. kompensator reads this directly from Docker.

### Reconciliation
The act of comparing desired vs. actual state and performing whatever changes are
needed to make actual match desired. Idempotent: running it when already in sync
does nothing.

### Drift
A difference between desired and actual state (e.g. Git says tag `abc123`, the node
runs `def456`). Drift triggers a deployment. Drift can also occur without a Git
change — e.g. a crashed container — which cron-driven reconciliation heals.

### Agent mode
kompensator running **on a node** (started by cron or via SSH). It does the actual
git pull, placement resolution, and Blue/Green deployment. Subcommand: `reconcile`.

### Controller / CLI mode
kompensator running **on the operator's workstation**. It reads the inventory,
fans out over SSH to trigger agents, and aggregates their status. Subcommands:
`apply`, `status`, `diff`, `bootstrap`, …

### Node
An SSH-reachable Linux host that runs Docker and a cron-scheduled `kompensator
reconcile`. Pre-provisioned with Docker, Git, a registry login, and a read-only
Git deploy key. Each node has a **node-local config** identifying itself and the
deployment repo(s) it follows.

### Node-local config
Configuration stored **on the node** under `~/.config/kompensator/` (created
manually in Phase 1, by `bootstrap` later). It declares the node's **name** and the
list of **deployment repos** it tracks. It contains no secrets.

### Deployment repo
A Git repository describing the desired state, placement, and compose files for one
or more environments. A node may follow more than one.

### Inventory
The list of nodes and their labels, stored in the Git repo. The controller resolves
selectors against the inventory to decide which nodes a command targets.

### Label
A `key=value` tag on a node (e.g. `role=app`, `env=preprod`, `region=eu`). Used by
both placement (which apps run here) and the CLI (which nodes a command targets).

### Selector
An app-side expression matching node labels (e.g. `role=app`). The app runs on every
node whose labels satisfy the selector.

### Placement
The computed mapping of apps → nodes, derived from selectors and labels. Each node
independently resolves *its own* placement during reconcile.

### Environment
A logical deployment target such as `preprod` or `prod`, each with its own desired
state, placement, and compose files.

### Blue/Green
A zero-downtime strategy *(Phase 2)*: deploy the new version to the inactive color
(blue or green), health-check it, then ask the proxy plugin to switch to it and stop
the old color. In Phase 1 the deployer uses a simpler **recreate** strategy.

### Proxy plugin
A pluggable load balancer integration *(Phase 2)*. kompensator notifies the proxy of
a color switch through a small interface, so multiple proxies can be supported. The
first plugin is **`haproxy-local`**, which notifies a locally running HAProxy. The
MVP interface offers **switch** ("app X → color Y on this node") and **get-config**
(print a ready-to-copy HAProxy backend definition).

### Secrets
App secrets and any sensitive `.env` values are placed **out of band** on the node
(not in Git). kompensator does not manage secrets.

### State write-back *(later phase)*
Optionally recording `last_deployed`, `deployed_by`, and `commit_sha` back into Git
as an audit trail. **Deliberately not part of Phase 1** — a node never writes to Git
in the initial implementation.

## Command vocabulary (proposed)

| Command | Mode | Purpose |
| --- | --- | --- |
| `kompensator reconcile <env>` | Agent | Pull Git, resolve placement, deploy on drift. Run by cron and over SSH. |
| `kompensator apply <env> [--select <labels>]` | Controller | Trigger reconcile on all (or selected) nodes in parallel. |
| `kompensator status <env>` | Controller | Query nodes live over SSH and show an aggregated state table. |
| `kompensator diff <env>` | Controller | Show desired vs. actual per node without changing anything. |
| `kompensator node add <node>` | Controller | Create config, clone deployment repo(s), install cron on the node. |
| `kompensator rollback <env>` | Controller | Switch nodes back to the previously active color. |
| `kompensator proxy get-config <env>` | Agent | Print an HAProxy backend definition to copy into HAProxy config. |
