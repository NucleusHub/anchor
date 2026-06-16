# Anchor — Architecture Specification

> **Status:** Design (validated). No implementation yet.
> Anchor is a privileged, break-glass **infrastructure control plane** for Nucleus.
> It is *not* a Nucleus app — it is shipped under `/apps/anchor` for organizational
> reasons only and is structurally excluded from the entire ecosystem (see §2).

---

## 1. Decisions of record

| Area | Decision |
|------|----------|
| Repo placement | `/apps/anchor`, excluded via a `nucleus.ignore` marker convention (§2) |
| Lifecycle | **Separate Docker Compose project** (`anchor`), independent of the `nucleus` project (§3) |
| Backend runtime | **Go, single static binary** (robust daemon, no runtime deps) |
| Docker access | **Socket-proxy sidecar** (filters API surface, blocks `/exec`) (§4) |
| State store | **Flat files** — `config` (0600) + append-only **JSONL audit log** (§9) |
| Design reuse | **Vendored copies** of needed `core/` components (no symlink) (§11) |
| Rebuild semantics | **Two distinct actions**: *Recreate* (from image) and *Rebuild from source* (§6) |
| Network reach | **Host-level Tailscale** + LAN; sidecar Tailscale as fallback (§10) |
| Auth | Single root user, **argon2id** hash in local file; no software reset (§8) |

---

## 2. Placement & ecosystem exclusion

Anchor lives at `/apps/anchor` but **must never appear** in any Nucleus registry,
ecosystem graph, generated compose/nginx config, hub symlink set, or Echo
integration scan.

### 2.1 The `nucleus.ignore` convention

A single shared rule is introduced across **all** discovery code:

> **Any directory containing a `nucleus.ignore` file is skipped entirely** —
> not scanned, not symlinked, not registered, not generated.

`apps/anchor/nucleus.ignore` is the marker. This replaces per-name denylists:
it is one rule, applied uniformly, and any *future* scanner inherits the guard
by using the shared helper.

### 2.2 Scanners that must honor it (prerequisite work)

All four current discovery paths must be taught the rule:

1. `infra/registry/index.js` — skip `apps/*` and `widgets/*` dirs with `nucleus.ignore`.
2. `infra/generate.js` — exclude such dirs from nginx/compose generation.
3. `infra/build` — exclude from the library-app symlink step (the empty
   `apps/anchor` currently matches the lib-app pattern; this guard prevents it
   from being symlinked into the hub).
4. Echo manifest loader + frontend glob — skip `apps/*/echo` under ignored dirs.

### 2.3 Regression guard

A test must assert Anchor never leaks: after running discovery/generation,
`anchor` must be absent from the registry output, the generated
`docker-compose.prod.yml`/`docker-compose.override.yml`, the generated
nginx configs, and the hub symlink set.

### 2.4 Trust-model honesty

Anchor is **root-equivalent over the host** (see §4). "Scoped only to Nucleus"
is enforced by **application-layer label filtering + the socket proxy**, not by
OS isolation. This is acceptable for a break-glass tool, but is stated plainly
rather than implied as a hard sandbox.

---

## 3. Deployment topology

Anchor runs as its **own Compose project**, separate from `nucleus`. This is the
single most important independence property:

- `docker compose -p nucleus down --remove-orphans` (which `infra/build` runs)
  **must not** touch Anchor.
- Anchor has its own compose file (e.g. `apps/anchor/deploy/docker-compose.yml`),
  started independently of `infra/build`.
- Anchor is **never** emitted into the generated Nucleus compose files.

```
┌─────────────────────── host ───────────────────────┐
│                                                     │
│  compose project: nucleus      compose project: anchor
│  ┌───────────────────────┐     ┌─────────────────────┐
│  │ nginx (:80/:443)      │     │ anchor (Go binary)  │
│  │ hub, *-server, *-client│    │   :8888 (host-bound)│
│  │ mongo, redis, minio    │    │        │            │
│  │ auth-server, registry  │    │        ▼            │
│  └───────────────────────┘     │ docker-socket-proxy │
│            │                    └─────────┬───────────┘
│            └──── reads labels via ────────┘
│                  proxy ──► /var/run/docker.sock
└─────────────────────────────────────────────────────┘
```

- **Containers:** `anchor` (control plane) + `docker-socket-proxy` (sidecar).
- **Restart policy:** `restart: unless-stopped` on both.
- **Resource limits:** CPU/memory caps on `anchor` so a bug cannot OOM the host
  it is meant to rescue.
- **No dependency** on mongo/redis/minio/nginx/auth-server.

---

## 4. Docker access (socket-proxy sidecar)

Anchor never mounts the raw socket. A filtering proxy
(e.g. Tecnativa `docker-socket-proxy`, HAProxy-based) sits in front of
`/var/run/docker.sock`.

### 4.1 Endpoint policy

| Capability | Endpoints | Policy |
|------------|-----------|--------|
| Discover/inspect | `containers` (list/inspect), `images`, `volumes`, `networks`, `events`, `version`, `info` | **Allow (read)** |
| Logs | container logs | **Allow** |
| Lifecycle | `POST start/stop/restart`, `containers/create`, `containers/{id}` (recreate) | **Allow (gated by Anchor auth + allowlist)** |
| Build | `build`, `images/create` (for *Rebuild from source*) | **Allow (gated)** |
| **Exec** | `/exec` | **BLOCKED at proxy** — this is what truly enforces "no arbitrary command execution"; a UI ban alone is bypassable |
| Swarm/secrets/configs/plugins | all | **BLOCKED** (out of scope) |

### 4.2 Defense in depth

Even though Anchor is trusted, **every mutating action** is validated in Anchor's
app layer against `nucleus.managed=true` on the target **before** the API call is
issued — belt-and-suspenders with the proxy's endpoint gating.

---

## 5. Service discovery model

### 5.1 Label taxonomy (replaces the 2-label scheme)

`generate.js` currently emits **no** container labels. It must be extended to
emit the following on every Nucleus container:

```
nucleus.managed = true                          # Anchor's primary filter
nucleus.stack   = nucleus                        # disambiguates if stacks ever coexist
nucleus.role    = app-server | app-client
                | database | cache | object-store
                | proxy | registry | auth         # component class
nucleus.app     = <app-id>                        # only for app-server / app-client
                                                  # absent (or "_shared") for infra
nucleus.depends = mongo,minio,...                 # optional; mirrors manifest `server.depends`
```

This lets Anchor: group the UI by app, render shared infra (mongo/redis/minio)
and the proxy separately, and **warn before stopping a shared service** that
other apps depend on (`nucleus.depends`).

### 5.2 Dynamic registry

- On boot: full `containers/list` (filtered by `nucleus.managed=true`) → registry.
- Live: subscribe to the Docker **event stream**, apply create/start/stop/die/destroy.
- **Resilience (required):** the event stream drops silently on daemon restart.
  Anchor must reconnect with backoff **and** run a **periodic full reconcile**
  (re-list) to repair missed events.
- **Docker is the single source of truth.** Anchor holds no authoritative service
  state — only its own config + audit. This is what makes "no rebuild when a new
  app is added" true: a new labeled container simply appears via events.

No grep, no naming heuristics, no CLI parsing — structured API + labels only.

---

## 6. Action system

All actions are **allowlisted**; there is no raw shell and no arbitrary command
endpoint.

| Action | Type | Mechanism |
|--------|------|-----------|
| Start service | non-destructive | `POST containers/{id}/start` |
| Stop service | **destructive** | `POST containers/{id}/stop` (warn on shared deps) |
| Restart service | non-destructive | `POST containers/{id}/restart` |
| View logs | read | stream logs |
| Inspect health | read | `containers/{id}/json` health field |
| **Recreate** | semi-destructive | recreate from existing image; **applies new `.env`**. Fast, self-contained. |
| **Rebuild from source** | **destructive/heavy** | invokes the existing `infra/build` pipeline as a subprocess (`--build`). Explicitly couples to Nucleus build tooling; clearly labeled as the heavy path. |
| Edit `.env` | **dangerous** | controlled filesystem write (§7) |
| Manage volumes | **destructive** | create/inspect/remove (delete behind double-confirm) |
| Manage images | semi-destructive | list/pull/remove |

### 6.1 Two rebuild modes (per decision)

- **Recreate** — `docker compose up -d --force-recreate <svc>` semantics; from the
  current image; cheap; the default break-glass "apply config & bounce cleanly."
- **Rebuild from source** — re-runs `infra/build` for the target; heavy; couples to
  repo layout + build pipeline; surfaced as a separate, more-guarded action with a
  cost/time warning.

### 6.2 Server-side destructive guards

UI confirmation prompts are client-side and bypassable. Destructive actions
(`stop`, `rebuild from source`, `delete volume`, system shutdown) use a **two-step
API**:

1. `POST .../prepare` → returns a short-lived confirmation token + an impact summary
   (e.g. "stopping `mongo` will affect: orbit, echo, goals, watchlist").
2. `POST .../execute` with that token → performs the action.

The UI prompt fills the gap between the two; the **server enforces** the confirmation.

---

## 7. `.env` editing (Anchor's most dangerous non-destructive power)

`infra/.env` is the central env file (also read by `build`, `generate.js`,
`nucleus`). Anchor's editing must be safe:

- **Backup-before-write:** retain the last *N* versions on Anchor's volume — there
  is currently no undo for a fat-fingered secret.
- **"Recreate to apply" semantics:** Docker bakes env at container *create* time.
  Editing `.env` then *restarting* does nothing. The UI must state that `.env`
  changes require **Recreate** (§6) to take effect, and offer to do so.
- **Secret hygiene:** values masked by default, reveal-on-demand, **never** written
  to the audit log in plaintext.
- **Single-writer:** file locking to avoid corruption from concurrent edits or a
  simultaneous `generate.js`/`build` read.
- **Scope:** Anchor may edit `.env`. It must **not** edit the *generated* configs
  (`nginx.conf`, `docker-compose.*.yml`) — those are owned by `generate.js` and
  would be overwritten. Anchor may restart/inspect the nginx container only.

---

## 8. Authentication & recovery

Fully independent of Nucleus auth (no Mongo `profiles`, no shared JWT secret).

- **Single root user.** Username fixed/implicit; one credential.
- **Hash:** **argon2id** (memory-hard), stored in Anchor's local config file at
  `0600` on Anchor's volume. (Independent of Nucleus's bcrypt PINs.)
- **First run:** if no hash file exists, Anchor serves a one-time setup screen to
  set the password, writes the hash `0600`, and never shows setup again.
- **Sessions:** signed session cookie bound to Anchor's origin; `Secure` set when
  served over Tailscale HTTPS (Nucleus omits `Secure` today — do **not** copy that);
  idle timeout; **re-auth required before destructive actions**.
- **Rate limiting:** per-attempt lockout/backoff on login. (Nucleus's limiter is
  keyed on IP-behind-proxy and effectively global — do **not** inherit that bug.)

### 8.1 Recovery (reframed)

There is **no in-app and no network reset** — by design. Recovery is a **manual,
filesystem-gated** operation:

> Delete (or replace) the hash file at `<anchor-volume>/config` via SSH/console,
> then restart Anchor → first-run setup re-triggers.

This *is* the reset mechanism; it is simply gated behind server access rather than
exposed in software. (No full reinstall is required — config/audit are preserved.)
The path and procedure must be documented in the runbook.

---

## 9. State & audit (flat files)

On Anchor's own volume, no external DB:

- **`config`** (`0600`): root password hash, session secret, UI preferences,
  Anchor's own settings.
- **`audit.jsonl`** (append-only): one JSON object per line.

```jsonc
// audit.jsonl entry
{
  "ts": "2026-06-17T12:00:00Z",
  "action": "recreate",
  "target": { "id": "…", "app": "orbit", "role": "app-server" },
  "params": { "force": true },         // secrets redacted
  "result": "ok",                       // ok | error
  "error": null,
  "confirmToken": "…"                  // links prepare→execute for destructive ops
}
```

- Append-only; secrets redacted before write; rotated by size with retained
  archives. This is the forensic + "what did I just break" record — mandatory for
  a privileged control plane.

---

## 10. Network & reachability

- Anchor **binds its own host port** (e.g. `:8888`) — **not** routed through the
  shared nginx. Reachable with zero dependency on Nucleus networking.
- **Tailscale:** host-level Tailscale is assumed, so the host port is reachable
  over the tailnet automatically and stays up independent of Docker. A
  Tailscale **sidecar in Anchor's own compose project** is the documented fallback
  (keeps the dependency inside Anchor's boundary, never the Nucleus stack).
- Reachable over **LAN** and **tailnet**. The direct URL is the canonical
  break-glass entry point and must be documented out-of-band.

---

## 11. Failure / fallback model

Two separate paths — do not conflate them:

### 11.1 Convenience path (best-effort)

When the hub upstream fails, nginx serves a static degraded-mode page linking to
Anchor's direct URL.

- Implemented via `error_page 502 503 504` on the catch-all `/` location.
- **Authored in the `generate.js` template** (not hand-edited) so it survives
  regeneration.
- Covers **upstream failure only** (hub 502/503/504 while nginx itself is healthy).
- Anchor is **never** part of automatic request routing — the page contains a
  *manual* link only.

### 11.2 Canonical path (authoritative)

Anchor's **direct host port** (§10). Works when nginx, the daemon, or the whole
Nucleus stack is down — i.e. the real break-glass surface. Operators must know
this URL; the nginx link is just a shortcut for the common case.

---

## 12. UI model

- Web UI only; no CLI required.
- **Vendored** copies of the specific `core/` pieces Anchor uses (`AppHeader`,
  `BackgroundBlobs`, theme, liquid-glass styles) — Anchor stays self-contained and
  out of the symlink/ecosystem graph. Trade-off accepted: occasional manual drift
  from upstream `core/`.
- **Semantically infra-oriented**, visually Nucleus-consistent. Priorities:
  - system state visibility (all services, health, at a glance)
  - service health & dependency awareness
  - recovery actions, prominent and unambiguous
  - operational clarity under failure (must be usable when everything else is down;
    no dependency on Nucleus assets/CDN/fonts loading)
- Destructive actions visually distinct; impact summaries from the `prepare` step
  shown inline; safe defaults for non-destructive actions.

---

## 13. Robustness (it is a daemon)

- Single static Go binary; no node_modules/runtime to break.
- `restart: unless-stopped`, own healthcheck, CPU/memory limits.
- Minimal deps: local files only; no Mongo/Redis (those are things it *manages*).
- Must boot and remain usable when the rest of the host is degraded.

---

## 14. Implementation prerequisites (Nucleus-side, before Anchor is useful)

These are changes to **existing** Nucleus tooling, independent of building Anchor:

1. ✅ **`nucleus.ignore` guard** added to all scanners (§2.2) — **done in Phase 1.**
   Applied in `infra/generate.js` (`readManifests` + `findHubLibraries`),
   `infra/registry/index.js`, and `infra/build` (lib-app loop + standalone-client
   find). Marker file: `apps/anchor/nucleus.ignore`.
2. ✅ **`generate.js` label emission** — the taxonomy in §5.1 — **done in Phase 1.**
   Labels emitted by `generate.js` (`prodServerBlock` + infra blocks), the base
   `infra/docker-compose.yml`, the `infra/create-app.js` scaffold template, and
   backfilled into all existing `apps/*/docker-compose.app.yml`.
3. ⬜ **nginx `error_page` → static fallback** added to the `generate.js` template
   (§11.1). **Deferred to Phase 4** (failure-mode UI) — it has no consumer until
   Anchor is reachable to link to.

### Phase 1 status (foundation — landed)

Phase 1 delivered the non-breaking foundation: ecosystem exclusion (item 1) and
the discovery-label substrate (item 2), with **zero change to the runtime
behavior** of the existing stack. The only observable effect is a one-time
container recreate on the next deploy, caused by the added (inert) labels.
The Go daemon, socket-proxy, auth, actions, and UI remain Phase 2+.

---

## 15. Open / deferred

- Whether `nucleus.depends` is emitted as a label vs read from manifests at runtime
  (both viable; label is simpler for Anchor, manifest is single-source-of-truth).
- Audit log retention/rotation thresholds.
- Optional future MFA on the root user (single-user + full infra power is a
  standing risk; argon2id + rate-limit + re-auth-before-destructive is the v1 floor).
- Exact `infra/build` subprocess contract for *Rebuild from source* (args, working
  dir, streaming build output to the UI).
