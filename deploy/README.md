# Anchor deploy

Anchor runs as its **own** Docker Compose project (`anchor`) — entirely separate
from the `nucleus` project — so tearing Nucleus down
(`docker compose -p nucleus down`) never takes Anchor with it. See
[`../ARCHITECTURE.md`](../ARCHITECTURE.md) §3 (topology) and §4 (socket proxy).

## What's here

- `docker-compose.yml` — the `anchor` project:
  - **anchor** — the Go control-plane daemon, on host port **8888**.
  - **docker-socket-proxy** — a read-only Docker API gateway (Tecnativa).
    Phase 2 sets `POST=0` and `EXEC=0`, so the daemon **cannot** mutate anything
    or exec into containers. This is platform-level enforcement, not just UI.

## Run it

```bash
cd apps/anchor/deploy
docker compose up -d --build
```

Then open **http://<host>:8888** (or over Tailscale). First load prompts you to
set the root password (argon2id, stored at `/data/config.json`, 0600).

## What you can do in Phase 2 (read-only)

- See every Nucleus container discovered via the `nucleus.managed=true` label,
  grouped by app, with role / state / health / status — **live** (the list
  refreshes on Docker events via SSE).
- Open a service to view its **logs** (last 300 lines) and full **inspect** JSON.
- Review the **audit log** (logins, log/inspect views) — append-only JSONL at
  `/data/audit.jsonl`.

> **Prerequisite:** the Nucleus stack must be running **with the labels** added in
> Phase 1. If you deployed Nucleus before that, redeploy it once
> (`infra/build`) so its containers carry `nucleus.*` labels — otherwise Anchor
> discovers nothing.

## Not yet (Phase 3+)

Lifecycle actions (start / stop / restart / recreate), `.env` editing, and
rebuild-from-source — these require flipping specific proxy endpoints on and
adding server-side two-step confirmation guards. Phase 4 adds the nginx fallback
page and Tailscale HTTPS.

## Recovery

There is no software password reset. To recover access: stop the project, delete
`config.json` from the `anchor_data` volume on the host, and start again —
first-run setup re-triggers.
