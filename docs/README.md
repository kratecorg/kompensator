# kompensator — Documentation

> **kompensator** is a GitOps deployment tool for Docker Compose workloads across
> multiple SSH-reachable nodes — think "Flux for plain Docker hosts".
>
> The name is a pun on the German dub of *Back to the Future*, where the
> *Flux Capacitor* is called **"Fluxkompensator"**. kompensator continuously
> "compensates" the drift between the desired state in Git and the actual state
> running on your nodes.

## What it does

kompensator watches one or more Git repositories that describe the **desired state**
of your fleet (which image/tag should run for which app, on which nodes) and
reconciles the **actual state** on each node to match it. Deployments use Docker
Compose. Traffic switching during a deployment is handled by a pluggable
**load balancer plugin** (the first being `haproxy-local`).

## Documentation index

| Document | Description |
| --- | --- |
| [roadmap.md](roadmap.md) | Phased implementation plan (what to build first) |
| [architecture.md](architecture.md) | System architecture, components, and diagrams |
| [concepts.md](concepts.md) | Glossary and core concepts |
| [repository-layout.md](repository-layout.md) | Proposed layout of the GitOps deployment repo |

## Key design decisions (locked)

- **Language:** Go. Single statically-linked binary.
- **Execution model:** Hybrid pull/push, **no long-running daemon**.
  - Each node runs `kompensator reconcile` via **cron (~every minute)** → self-healing pull loop.
  - The operator's local **CLI** can trigger the same reconcile **immediately over SSH**.
- **Source of truth:** Git. Each node clones/pulls its configured deployment repo(s) itself using a read-only deploy key.
- **Node-local config:** Each node has its own config under `~/.config/kompensator/`
  holding the node **name** and the list of **deployment repos** it follows.
- **Runtime:** Docker Compose, **v2.26.0 or newer** on every node (kompensator uses
  `docker compose config --variables` to fingerprint a deploy). No fallback for
  older versions — a node either meets it or refuses to reconcile.
- **Load balancing:** A **pluggable proxy interface**; the first plugin is
  `haproxy-local` (notifies a locally running HAProxy of a color switch).
- **Topology:** Apps are placed on nodes via **labels/groups** (selectors).
- **Rollout coordination:** All matching nodes reconcile **in parallel**. Only
  Blue/Green (no rolling), so no cross-node gating is needed.
- **Scale:** Targets a realistic fleet of **1–4 nodes**; larger setups should use
  Kubernetes or similar.
- **Status:** CLI queries nodes **live over SSH** and aggregates.
- **Git pulls** use `git pull --rebase` (clean fast-forward; no jitter needed).
- **Secrets:** placed **out of band** on the node — never committed to Git.
- **Logging:** structured logs to **journald** (pick up via Loki etc.).
- **Node prerequisites:** Nodes are pre-provisioned (Docker with Compose >= 2.26.0, Git, registry login, deploy key all present).

## Implementation phases (summary)

kompensator is built incrementally — see [roadmap.md](roadmap.md) for details.

1. **Phase 1 — Detect & deploy.** A node detects that its deployment repo has a new
   version and deploys it. No load balancer, no write-back of state. Node config is
   created manually/locally.
2. **Phase 2 — Load balancer plugin.** Pluggable proxy interface with the
   `haproxy-local` plugin: a local HAProxy is notified of a Blue/Green switch.
3. **Phase 2–3 — Bootstrapping.** `kompensator bootstrap` creates the config folder,
   checks out the deployment repo(s) and installs the cron job — locally or remotely.
