# kompensator examples

Runnable, minimal example deployments that show the shape of a kompensator
**deployment repo** and the **node-local config**. They use only public images
(`traefik/whoami`, `nginxdemos/hello`) so nothing needs a private registry.

## The two kinds of configuration

kompensator always works with two separate things:

1. **Node-local config** — lives in a node's *kompensator home*
   (default `~/.config/kompensator/`). It is either:
   - `node.yml` — a node that follows one deployment repo and reconciles itself, or
   - `controller.yml` — an operator workstation that tracks repos and drives nodes over SSH.
2. **Deployment repo** — a Git repo describing the desired state. Each example
   ships one under `deployment/`:

   ```
   deployment/
   ├── inventory/nodes.yml              # the nodes this repo knows about
   ├── environments/<env>/
   │   ├── env.yml                      # which stacks run here + variables/placement
   │   └── state/<stack>.yml            # DESIRED image tags (what CI bumps)
   └── stacks/<stack>/
       ├── stack.yml                    # projects, strategy, networks, proxy
       └── compose/*.yml                # the Docker Compose files
   ```

Image tags from `state/<stack>.yml` are injected into the compose files as
`<SERVICE>_IMAGE` / `<SERVICE>_TAG` (the service name, upper-cased). kompensator
also injects `NODE_NAME`, `ENV_NAME`, and — for Blue/Green deploys — `COLOR`.

## The examples

| Example | Shows |
| --- | --- |
| [01-single-node](01-single-node/) | The smallest possible deploy: one node, one stack, `recreate` strategy, a published port |
| [02-blue-green](02-blue-green/) | Zero-downtime Blue/Green behind a kompensator-managed Traefik proxy |
| [03-multi-node](03-multi-node/) | A controller driving several nodes with stack/project **placement** pinning |

Each example has its own `README.md` with a walkthrough.

## Running an example

kompensator reconciles a node against a deployment repo. For a purely local
try-out on one host:

1. Publish the example's `deployment/` directory as a Git repo the node can
   reach (a local `file://` clone works), or point the node at your fork.
2. Put the example's `node.yml` in a kompensator home and set the repo `url` to
   that Git repo.
3. Reconcile:

   ```bash
   KOMPENSATOR_HOME=/path/to/home kompensator reconcile dev
   ```

> These configs are illustrative and intentionally minimal. Adjust node
> locations, hostnames, and ports for your environment.
