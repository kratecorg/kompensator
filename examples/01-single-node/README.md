# Example 01 — single node, recreate

The smallest complete kompensator setup: **one node**, **one stack** (`web`)
with **one project** (`app`) running [`traefik/whoami`](https://hub.docker.com/r/traefik/whoami)
with the `recreate` strategy, publishing a host port.

```
single node ── stack "web" ── project "app" ── service "whoami" → :8080
```

## What each file does

| File | Role |
| --- | --- |
| [node.yml](node.yml) | Node-local config: this node follows the `example` repo |
| [deployment/inventory/nodes.yml](deployment/inventory/nodes.yml) | The one node kompensator knows about |
| [deployment/environments/dev/env.yml](deployment/environments/dev/env.yml) | `dev` runs the `web` stack; sets `WEB_PORT` |
| [deployment/environments/dev/state/web.yml](deployment/environments/dev/state/web.yml) | Desired image + tag for the `whoami` service |
| [deployment/stacks/web/stack.yml](deployment/stacks/web/stack.yml) | The `web` stack: one `recreate` project |
| [deployment/stacks/web/compose/app.yml](deployment/stacks/web/compose/app.yml) | The Compose file |

## Try it

1. Make `deployment/` reachable as a Git repo and point `node.yml`'s repo `url`
   at it.
2. Reconcile the `dev` environment:

   ```bash
   KOMPENSATOR_HOME=/path/to/home kompensator reconcile dev
   ```

3. Verify the service answers:

   ```bash
   curl -s localhost:8080 | head
   ```

## Deploy a new version

Bump the `tag` in [state/web.yml](deployment/environments/dev/state/web.yml) and
reconcile again — kompensator detects the drift and recreates the container with
the new image.
