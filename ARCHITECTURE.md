# DevStrap Architecture

This is the middle tier of DevStrap's documentation: longer than the README, shorter than
the design corpus under [`spec/`](spec/). It explains *why* DevStrap is shaped the way it is
and how the pieces fit together. If you want to use DevStrap, start with
[`docs/quickstart.md`](docs/quickstart.md); if you want the full design rationale for any one
subsystem, each section below ends with a **Depth** pointer into `spec/`.

## The core decision: a managed physical namespace, not a virtual filesystem

`~/Code` is a **real folder**. DevStrap owns its structure and metadata; Git owns repository
content; a local state database records where every project lives. There is no FUSE mount, no
placeholder files, and no lazy virtual filesystem — after a sync the tree is really present on
disk, and every editor, shell, and build tool sees ordinary directories.

A true lazy virtual filesystem (mounting `~/Code` and materializing files on access) is
attractive, but it is the hardest possible thing to build first: kernel/system extensions,
Finder and File Provider integration, caching, file locking, and editor-indexer edge cases,
all before the product loop is even proven. DevStrap defers that layer — called **StrapFS** —
indefinitely, and gets a Dropbox-like "same tree everywhere" experience the boring way: by
reconstructing the same real directories on every machine.

**Depth:** [`spec/01_ARCHITECTURE_DECISION.md`](spec/01_ARCHITECTURE_DECISION.md).

## The Workspace Passport: eager whole-tree materialization

The product promise is one command on a fresh machine:

> Install DevStrap → point it at `~/Code` → authenticate Git and secrets → run
> `devstrap sync` once → the whole tree is reconstructed.

Materialization is **eager, not lazy**. A single `devstrap sync` walks the namespace map and,
for every project, blobless-clones the repo (`git clone --filter=blob:none`) from its existing
remote, pulls any non-git/draft content from encrypted blobs, and hydrates env profiles. There
is no skeleton to "open" first — the folders are on disk when sync returns.

Two things are deliberately never synced: `.git` internals (file-syncing them corrupts the
repo — repo content instead rides Git's own transport) and `node_modules`/build artifacts
(rebuilt on hydrate, never carried across the wire).

**Depth:** [`spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md`](spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md)
and [`spec/00_START_HERE.md`](spec/00_START_HERE.md).

## Components

```text
┌─────────────────────────────────────────────────────────────┐
│  devstrap CLI          shell hooks / editor adapters         │  user-facing
└───────────────┬─────────────────────────────────────────────┘
                │ (a local daemon `devstrapd` is planned, not built)
                ▼
┌─────────────────────────────────────────────────────────────┐
│  Local engine (today: Cobra command closures in internal/)   │
│   namespace reconciler · Git materializer · worktree manager │
│   env/secret broker · ignore compiler · device sync client   │
└───────────────┬─────────────────────────────────────────────┘
                │
     ┌──────────┼───────────────┬───────────────┐
     ▼          ▼               ▼               ▼
  SQLite     ~/Code          Git remotes      DevStrap Hub
  state      (real tree)     (GitHub/…)       (two-plane, zero-knowledge)
```

Local state lives under `~/.devstrap/`: a `0600` WAL SQLite database (`state.db`), the
age-encrypted env/draft blob cache, device key material (OS keychain preferred, `0600` file
fallback for headless/CI), and managed worktrees.

**Depth:** [`spec/03_SYSTEM_ARCHITECTURE.md`](spec/03_SYSTEM_ARCHITECTURE.md).

## The two-plane, zero-knowledge hub

DevStrap never blanket-syncs a folder. Sync is split by content type across two planes, and
the hub only ever sees ciphertext plus a signed map — it cannot read your code, secrets, or
drafts.

- **Plane A — the namespace map.** An append-only, Ed25519-signed, HLC-ordered event log: the
  map of every project (path, remote, env profile, policy) plus tombstones. Each device
  replays from its last cursor to reconstruct the identical `~/Code` tree. Event *payloads* are
  additionally envelope-encrypted at the hub boundary under a per-epoch Workspace Content Key,
  so even the map is opaque to the carrier.
- **Plane B — the encrypted blob store.** Content-addressed `age_blob:<sha256>` blobs holding
  env profiles and non-git/draft content, encrypted client-side to the enrolled device
  recipient set before upload.

Repo content rides Git's own transport and never touches the hub. That two-plane split is what
makes any "dumb" storage a safe zero-knowledge boundary.

**Carriers.** The same object set rides three interchangeable backends behind one pluggable
`Hub` interface:

- **git carrier (the default)** — `hub: git+ssh://…` (or `hub init <git-url>`). Any private
  Git repo you can already push to becomes the hub. No bucket, no new credential plane — your
  existing SSH key / credential helper. Zero infrastructure.
- **folder carrier** — `hub: folder:<abs-path>`. A shared directory (Dropbox/iCloud/Drive
  folder, SMB/NFS mount); the drive itself is the replication transport.
- **R2/S3** — `hub: r2://<bucket>`. Reach for this at scale: blobs over a forge's per-object
  limit, higher push rates, or object-storage economics.

**Depth:** [`spec/03_SYSTEM_ARCHITECTURE.md`](spec/03_SYSTEM_ARCHITECTURE.md) (Hub backends),
[`spec/07_NAMESPACE_AND_SYNC_MODEL.md`](spec/07_NAMESPACE_AND_SYNC_MODEL.md), and
[`docs/self-hosting.md`](docs/self-hosting.md) for operating one.

## Compaction and snapshot bootstrap

An append-only log would grow without bound and a fresh device would have to replay all of
history (including retired key epochs). `devstrap hub compact` publishes a sealed full-state
snapshot, advances a signed per-device retention floor, and deletes the now-cold events below
it. A device whose cursor has fallen below the floor recovers automatically on its next sync by
importing the snapshot instead of replaying from zero, and a brand-new device bootstraps the
same way. Tombstones are garbage-collected only once every approved device has acked them, so a
compaction can never resurrect a deleted project on a lagging device.

**Depth:** [`spec/07_NAMESPACE_AND_SYNC_MODEL.md`](spec/07_NAMESPACE_AND_SYNC_MODEL.md).

## Device trust and key custody

A workspace is a set of devices that share one workspace id. The founder mints the id at
`init`; every later device *adopts* it. Pairing is a two-paste ceremony (each device shows the
other a `devstrap-pair1:` code) plus one out-of-band fingerprint read in each direction — the
code is a non-secret prefix selector, but the fingerprint, read over a trusted channel, is what
authorizes the keys. Each device holds age X25519 (encryption) and Ed25519 (signing)
identities; private keys stay in the OS keychain (or a `0600` file for headless/CI), never in
SQLite or config. Revoking a device re-encrypts affected blobs to the reduced recipient set and
flags the exposed secrets for rotation; the workspace key also rotates automatically once its
epoch ages past a threshold.

**Depth:** [`spec/07_NAMESPACE_AND_SYNC_MODEL.md`](spec/07_NAMESPACE_AND_SYNC_MODEL.md) and
[`spec/15_SECURITY_THREAT_MODEL.md`](spec/15_SECURITY_THREAT_MODEL.md).

## Agent workspaces

DevStrap's durable value is as the **substrate agents run on**, not as an agent runner itself.
Every agent task gets a fresh worktree created from the *fetched* `origin/<default_branch>` —
never a stale local branch — with the base SHA recorded so `agent pr` can refuse to push work
built on a moved base. Runs are logged to a `0600` file and tracked in a queryable registry.

The wrapper's command/file policy is **guardrails, not a sandbox** — it layers beneath an
OS-enforced sandbox on both platforms (`--sandbox auto|off|require`). On macOS a Seatbelt profile
wraps the child: writes are confined to the worktree, credential paths (`~/.ssh`, `~/.aws`, …) are
denied, and network is blocked for read-only policies. On Linux the child runs under bubblewrap,
falling back to Landlock+seccomp where user namespaces are restricted; the Landlock path keeps
credential reads readable and says so at run start.

**Depth:** [`spec/10_AGENT_WORKSPACES_AND_POLICIES.md`](spec/10_AGENT_WORKSPACES_AND_POLICIES.md).

## What is deliberately not built

DevStrap is honest about its edges. Not yet built, by design:

- **The local daemon** (`devstrapd`), its socket API, and an FSEvents-specific Mac watcher —
  every CLI command works correctly without a daemon; local reconciliation is the explicit
  `devstrap scan` plus the portable `run-loop` (which `devstrap service install` already wraps in
  an unattended LaunchAgent/systemd user unit).
- **StrapFS** — the optional lazy virtual filesystem, deferred until the product loop is proven.
- **A bespoke HTTP/SSE relay** and a hosted control plane for production device enrollment —
  the git/folder/R2 carriers cover the transport today.

**Depth:** the staged plan and open backlog live in
[`spec/14_MVP_ROADMAP_AND_BACKLOG.md`](spec/14_MVP_ROADMAP_AND_BACKLOG.md); the audit archive
is under [`docs/audits/`](docs/audits/).
