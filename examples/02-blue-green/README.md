# Example 02 — Blue/Green behind a managed Traefik

Zero-downtime deploys with the `blue-green` strategy. kompensator deploys the
new color (blue or green), waits for it to become healthy, flips a
**kompensator-managed Traefik** to the new color, then stops the old one.

```
managed Traefik ──► frontend-blue  ┐
        (route flips atomically)   ├─ only one color serves at a time
                └──► frontend-green ┘
```

The Traefik here is **managed**: kompensator synthesizes and runs it from the
`proxy:` block in [stack.yml](deployment/stacks/web/stack.yml) — there is no
hand-written compose file for it. It joins the stack's shared networks and, like
in production, does **not** publish a host port itself: a real setup fronts it
with an external edge proxy that reaches it under the network alias `web-traefik`.

## What each file does

| File | Role |
| --- | --- |
| [deployment/inventory/nodes.yml](deployment/inventory/nodes.yml) | The one node |
| [deployment/environments/dev/env.yml](deployment/environments/dev/env.yml) | `dev` runs the `web` stack |
| [deployment/environments/dev/state/web.yml](deployment/environments/dev/state/web.yml) | Desired image + tag for `frontend` |
| [deployment/stacks/web/stack.yml](deployment/stacks/web/stack.yml) | Networks, the managed Traefik `proxy:` block, and a `blue-green` project with a route |
| [deployment/stacks/web/compose/app.yml](deployment/stacks/web/compose/app.yml) | Compose file; registers the `frontend-${COLOR}` network alias and a healthcheck |

## Key points

- The project's `proxy:` binding (`router`, `service`, `port`, `rule`) tells the
  managed Traefik what to route. kompensator rewrites the route to the healthy
  color on every switch.
- The compose service registers the alias `frontend-${COLOR}` so both colors can
  run side by side during a switch (no shared host port).
- A working **healthcheck** is required — it is how kompensator knows the new
  color is ready before flipping traffic.

## Deploy a new version

Bump the `tag` in [state/web.yml](deployment/environments/dev/state/web.yml) and
reconcile. kompensator brings up the *other* color with the new image, waits for
health, switches the route, and tears the old color down.
