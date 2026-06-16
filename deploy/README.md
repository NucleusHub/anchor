# Anchor deploy (placeholder)

Anchor runs as its **own** Docker Compose project (`anchor`) — entirely separate
from the `nucleus` project — so that tearing Nucleus down
(`docker compose -p nucleus down`) never takes Anchor with it. See
[`../ARCHITECTURE.md`](../ARCHITECTURE.md) §3 (topology) and §4 (socket proxy).

This directory will hold Anchor's compose file and the socket-proxy sidecar
config. **Added in Phase 2** — nothing here yet.

Anchor is excluded from all Nucleus discovery via the `nucleus.ignore` marker in
the parent directory, so it will never be emitted into the generated
`docker-compose.*.yml`.
