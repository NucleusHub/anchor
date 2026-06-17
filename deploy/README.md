# Anchor deploy

Anchor runs as its **own** Docker Compose project (`anchor`) — entirely separate
from the `nucleus` project — so tearing Nucleus down
(`docker compose -p nucleus down`) never takes Anchor with it. See
[`../ARCHITECTURE.md`](../ARCHITECTURE.md) §3 (topology) and §4 (socket proxy).

## What's here

- `docker-compose.yml` — the `anchor` project:
  - **anchor** — the Go control-plane daemon, on host port **8888**.
  - **docker-socket-proxy** — a Docker API gateway (Tecnativa). Phase 3 allows
    `POST`/`BUILD` (for lifecycle/recreate/rebuild) but keeps `EXEC=0`, so no
    command can actually be executed inside a container.

## Run it

```bash
cd apps/anchor/deploy
NUCLEUS_REPO=$(cd ../../.. && pwd) docker compose up -d --build
```

`NUCLEUS_REPO` is the **absolute host path** of the Nucleus repo. Anchor mounts it
at that same path inside the container so `docker compose` recreate/rebuild
resolves the project's relative build contexts and bind mounts to paths the host
daemon can see. **If you omit it**, read-only + lifecycle still work, but
recreate / rebuild / `.env` editing are disabled.

Then open **http://<host>:8888** (or over Tailscale). First load prompts you to
set the root password (argon2id, stored at `/data/config.json`, 0600).

> **Prerequisite for discovery:** the Nucleus stack must be running **with the
> labels** from Phase 1. If you deployed Nucleus before that, redeploy once
> (`infra/build`) so its containers carry `nucleus.*` labels.

## What you can do

- **Discover** every Nucleus container via the `nucleus.managed=true` label,
  grouped by app, with role / state / health — **live** (SSE on Docker events).
- **Logs** (last 300 lines) and full **inspect** per service.
- **Lifecycle:** start / restart (non-destructive); **stop** (destructive).
- **Recreate** (apply new `.env` / config from the existing image) and
  **Rebuild** (rebuild the image from source) — via `docker compose`.
- **Edit `.env`** — backup-before-write; changes apply after a recreate.
- All **destructive actions require a two-step server-side confirmation**; every
  action is recorded in the append-only **audit log** (`/data/audit.jsonl`).

## Not yet (Phase 4)

nginx `error_page` degraded-mode fallback, Tailscale HTTPS (`ANCHOR_SECURE`),
exec-create ACL hardening, and optional secret masking in the `.env` editor.

## Recovery

There is no software password reset. To recover access: stop the project, delete
`config.json` from the `anchor_data` volume on the host, and start again —
first-run setup re-triggers.
