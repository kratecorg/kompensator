# Example 03 — multi-node placement

An operator **controller** drives three nodes over SSH, and the deployment repo
uses **placement** to pin parts of a stack to specific nodes.

```
controller ──ssh──► node-a  (runs: web/frontend)
           ──ssh──► node-b  (runs: web/frontend)
           ──ssh──► node-db (runs: web/cache only)
```

## What each file does

| File | Role |
| --- | --- |
| [controller.yml](controller.yml) | Operator config: tracks the `example` repo, drives nodes |
| [deployment/inventory/nodes.yml](deployment/inventory/nodes.yml) | Three SSH-reachable nodes |
| [deployment/environments/prod/env.yml](deployment/environments/prod/env.yml) | Placement: `frontend` on the app nodes, `cache` on the db node |
| [deployment/environments/prod/state/web.yml](deployment/environments/prod/state/web.yml) | Desired tags for both services |
| [deployment/stacks/web/stack.yml](deployment/stacks/web/stack.yml) | Two projects: `frontend` and `cache` |
| [deployment/stacks/web/compose/*.yml](deployment/stacks/web/compose/) | The Compose files |

## How placement works

The inventory lists nodes; it carries **no** environment membership. The
environment's [env.yml](deployment/environments/prod/env.yml) decides who runs
what. A stack listed as a bare name runs on every node of the environment; a
mapping with `nodes:` (at the stack or per-project level) narrows it:

```yaml
stacks:
  - name: web
    projects:
      - name: frontend
        nodes: [node-a, node-b]   # app tier only
      - name: cache
        nodes: [node-db]          # cache pinned to the db node
```

Each node independently resolves *its own* placement during `reconcile`, so the
controller just fans the same command out to all of them.

## Try it

From the controller host:

```bash
# see desired vs. running across all nodes
kompensator status prod

# reconcile every node in parallel over SSH
kompensator reconcile prod
```
