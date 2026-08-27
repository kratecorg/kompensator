# kompensator

[![CI](https://github.com/kratecorg/kompensator/actions/workflows/ci.yml/badge.svg)](https://github.com/kratecorg/kompensator/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8.svg)](go.mod)

> GitOps deployment for Docker Compose workloads across multiple SSH-reachable
> nodes — think "[Flux](https://fluxcd.io/) for plain Docker hosts".

The name is a pun on the German dub of *Back to the Future*, where the
*Flux Capacitor* is called **"Fluxkompensator"**. kompensator continuously
"compensates" the drift between the desired state in Git and the actual state
running on your nodes.

## What it does

kompensator watches one or more Git repositories that describe the **desired state**
of your fleet (which image/tag should run for which app, on which nodes) and
reconciles the **actual state** on each node to match it. Deployments use Docker
Compose. Traffic switching during a Blue/Green deploy is handled by a pluggable
**proxy plugin**.

- **No daemon.** Each node runs `kompensator reconcile` from cron (~every minute)
  for a self-healing pull loop. The operator's CLI can trigger the same reconcile
  immediately over SSH.
- **Single binary, two roles.** The same binary is the node agent and the
  controller/CLI; the role is detected from the local config.
- **Git is the source of truth.** Each node pulls its configured deployment repo
  with a read-only deploy key. Secrets stay out of Git (age-encrypted, delivered
  out of band).
- **Small fleets.** Targets a realistic 1–4 node setup; larger deployments should
  use Kubernetes or similar.

## Status

kompensator is **in active development**. It is built incrementally in phases —
see [docs/roadmap.md](docs/roadmap.md) for what is implemented and what is planned.

## Install

### `go install`

```bash
go install github.com/kratecorg/kompensator/cmd/kompensator@latest
```

### From source

Requires Go 1.24+.

```bash
git clone https://github.com/kratecorg/kompensator.git
cd kompensator
make build        # produces ./bin/kompensator
```

### Pre-built binaries

Download the binary for your platform from the
[Releases](https://github.com/kratecorg/kompensator/releases) page.

## Usage

```
kompensator [global flags] <command> [args]
```

Common commands:

| Command | Purpose |
| --- | --- |
| `reconcile [env [stack [project]]]` | Pull the deployment repo(s) and deploy on drift |
| `status [env]` | Show desired vs. running images |
| `pause` / `resume` | Suspend/resume reconciling during a delicate operation |
| `verify <env>` | Check from Git that every node reached the desired commit and is healthy |
| `check` | Audit a node/controller bootstrap |
| `bootstrap` | Provision a new node from the controller |
| `secrets …` | Manage age-encrypted environment and file secrets |
| `version` | Print the version |

Run `kompensator help` for the full command reference.

## Documentation

| Document | Description |
| --- | --- |
| [docs/README.md](docs/README.md) | Documentation index |
| [docs/architecture.md](docs/architecture.md) | System architecture, components, and diagrams |
| [docs/concepts.md](docs/concepts.md) | Glossary and core concepts |
| [docs/repository-layout.md](docs/repository-layout.md) | Layout of a GitOps deployment repo |
| [docs/roadmap.md](docs/roadmap.md) | Phased implementation plan |

## Development

```bash
make build   # build ./bin/kompensator
make test    # run tests
make race    # run tests with the race detector (needs a C compiler)
make vet     # go vet
```

## License

[MIT](LICENSE) © kratec GmbH
