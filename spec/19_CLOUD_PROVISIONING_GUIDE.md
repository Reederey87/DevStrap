---
last_reviewed: 2026-07-15
tracks_code: [internal/hub/**, internal/cli/hub.go, internal/cli/durability_export.go, internal/cli/doctor.go, internal/cli/run_loop.go, internal/cli/sync.go]
---
# Cloud Provisioning & Configuration Guide

> **Status: the single-owner R2/S3 recipe is shipped (`P5-HUB-01`); the multi-tenant
> SaaS direction (`SCALE-*`, Fly.io compute, managed Postgres control plane) remains
> FUTURE.** This guide is the provisioning runbook for both: it doubles as a single-owner
> deployment recipe usable today and the SaaS hosting direction for later. The shipped sync
> transport is `devstrap sync` with `hub: git+ssh://…` / `git@host:path.git` (the
> zero-infrastructure git carrier — the documented default quickstart) or `hub: r2://<bucket>`
> (the `aws-sdk-go-v2` S3 adapter — this guide's scale/power path; `--hub-file <path>`
> stays for tests). Env/config keys below marked *shipped* are live;
> those marked *planned* name the intended SaaS surface and are provisional until `SCALE-*`
> ships. See `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` (decisions 5 and 6) and
> `14_MVP_ROADMAP_AND_BACKLOG.md`.

> **Zero-infrastructure quickstart (AD-1) — the git carrier is SHIPPED (2026-07-04).**
> Provisioning an R2 bucket is real first-run friction. Because the hub only ever holds
> ciphertext plus signed events, even a "dumb" carrier is a safe zero-knowledge boundary —
> and you no longer need a bucket at all: point DevStrap at any **private git repository**
> you can already push to:
>
> ```yaml
> # config.yaml
> hub: git+ssh://git@github.com/you/devstrap-hub.git   # or git+https://…, git@host:path.git
> ```
>
> Create an empty private repo (any forge or bare ssh host), set `hub:` as above, and run
> `devstrap sync` — the first push seeds the carrier (a `devstrap-hub.json` marker plus the
> same encrypted object layout R2 stores). Requirements: non-interactive git auth (an
> agent-loaded ssh key or stored https credentials — git runs with prompts disabled) and a
> **private** repo (contents are ciphertext, but repo metadata/sizes are visible to the
> host). `?branch=<name>` selects a carrier branch (default `main`). Run `hub compact`
> periodically — it is what bounds the carrier repo's history (a squashed rewrite the host
> then garbage-collects) — and mind forge object limits (GitHub: 100 MB/object hard cap,
> so larger env/draft blobs need an S3-compatible hub, and ~2 GiB/push). The git carrier
> **is the documented default quickstart** since the `AD-1` swap (2026-07-04, README +
> `init` hints; forge-proven by the §F.2 live dogfood); the rest of this guide (provisioned
> R2/S3) is the *scale/power / self-hosting* path. Remaining `AD-1` slices
> (`hub init <git-url>` bootstrap, folder carrier) are tracked in `14_MVP_ROADMAP_AND_BACKLOG.md`.

## Scope

This file explains how to register, create, scope, and configure the three external
platforms DevStrap's cloud direction depends on, and how to hand their credentials to
DevStrap **only** through the existing encrypted secrets path — never as plaintext in
config, logs, or git. It does not change the architecture; for the why behind each choice
see the cross-references in the last section.

A first-time reader should skim *The stack at a glance*, then follow sections A → B → C in
the order given by *Cross-cutting: provisioning order & checklist*.

## The stack at a glance

```text
Repo content        -> each repo's existing git remote   (never touches the cloud stack)
Namespace map       -> Cloudflare R2  (signed, HLC-ordered event log)        [Plane A]
Env + draft content -> Cloudflare R2  (age_blob:<sha256> ciphertext store)   [Plane B]
Control-plane data  -> Neon Postgres  (accounts, devices, billing, tenancy)
Compute (both planes)-> Fly.io        (control-plane API + agent-runner microVMs)
```

| Platform        | DevStrap role                                          | Plane              | Sees plaintext? |
| --------------- | ----------------------------------------------------- | ------------------ | --------------- |
| Cloudflare R2   | Sync **hub data plane** — event log + encrypted blobs | Data plane         | No (ciphertext + signed map only) |
| Neon (Postgres) | **Control-plane DB** — accounts, devices, billing, metering, tenant directory | Control plane | Yes (non-secret control metadata only) |
| Fly.io          | **Compute** for both planes + ephemeral runner microVMs | Control + runners | Transiently, inside the microVM only |

Two invariants hold across the whole stack:

1. **Repo content never enters the cloud stack.** It rides each repository's own git
   transport (blobless `git clone --filter=blob:none` / fetch) from its existing remote.
   R2 carries only the signed namespace map and `age_blob:<sha256>` ciphertext.
2. **R2 is zero-knowledge for confidentiality.** Everything stored there is client-side
   age-encrypted and signed before upload, so R2 holds ciphertext plus a signed map and
   can read neither code, secrets, nor drafts. Tenant confidentiality for the eventual
   SaaS falls out of the encryption model. Integrity and availability still require
   scoped credentials, signed hash-chain verification, snapshots/backups, retention
   discipline, and budget/rate controls (`15_SECURITY_THREAT_MODEL.md`).

---

## A. Cloudflare R2 — storage / hub data plane

R2 is the production backend for the two-plane zero-knowledge hub: an append-only,
signed, HLC-ordered **event log** (the namespace map) and a content-addressed **encrypted
blob store** (env values and non-git/draft content as `age_blob:<sha256>`). It is chosen
for the S3-compatible API, **zero egress fees**, and a generous free tier.

### A.1 Sign up and enable R2

1. Create a Cloudflare account at `https://dash.cloudflare.com` and verify the email.
2. In the dashboard open **R2 Object Storage** and enable it. R2 requires a payment method
   on file even though usage stays inside the free tier for a solo deployment (see *Cost*).

### A.2 Create the bucket

1. **R2 → Create bucket.** Name it `devstrap-hub` (any name works; this guide uses
   `devstrap-hub` throughout).
2. Pick a location hint near the fleet, or leave it automatic. No public access; no custom
   domain. **No CORS configuration is needed** — DevStrap reaches R2 server-side over the
   S3 API, not from a browser.

For a solo or pooled deployment, one bucket can hold every workspace. Tenants/workspaces
are separated by **key prefix**; dedicated buckets or BYOC buckets remain options for
regulated or large tenants:

```text
s3://devstrap-hub/workspaces/<workspace_id>/eventlog/<device_id>/<seq pad20>_<event_id>.json
s3://devstrap-hub/workspaces/<workspace_id>/events/<hlc-padded>/<device_id>/<seq>/<event_id>.json  # RETIRED layout, dual-READ only (pre-P5-SYNC-01 hubs)
s3://devstrap-hub/workspaces/<workspace_id>/blobs/<sha256>
s3://devstrap-hub/workspaces/<workspace_id>/snapshots/<sha256>.json      # sealed full-state snapshot objects, content-addressed (P4-SYNC-02/P4-HUB-11)
s3://devstrap-hub/workspaces/<workspace_id>/meta/retention.json          # signed per-device retention-floor manifest, CAS-guarded (P6-HUB-04)
```

The earlier reservation for `snapshots/<hlc-padded>.json.age` is retired: snapshot
objects are sealed under the current-epoch **Workspace Content Key** (the same
XChaCha20-Poly1305 plane as `enc.v2` events), not age-wrapped to per-device
recipients — WCK grants already solve group access with no per-device re-wrap on
enrollment, and sealing under the current epoch makes each compaction a natural
retirement boundary for old-epoch ciphertext. Content addressing (sha256 of the
sealed bytes) replaces HLC keying so concurrent compactors can never clobber each
other's objects; the signed retention manifest names the current snapshot and is
the single mutable head object, written with If-Match/If-None-Match
compare-and-swap (an S3 extension R2 supports on PUT).

`<workspace_id>` is a `ws_<uuidv7>` identity minted on the **founder** device during
`devstrap init`. A second device does **not** mint its own — it **adopts** the founder's id
with `devstrap init --join --code <devstrap-pair1:...>` (or the manual fallback
`--workspace-id <id>`), so both devices key under the same
`workspaces/<workspace_id>/` prefix and see each other. A device that initializes with its
own fresh id keys a disjoint prefix and never observes the founder's content; the fix is the
born-correct join, not a post-hoc rewrite (see *E. Pair a second device*). Because every
object under a prefix is already encrypted and the map signed, prefix-level separation is
sufficient for confidentiality. Access scoping (below) is still required for integrity and
availability: a bucket-wide key can delete or withhold ciphertext even though it cannot
decrypt it.

Event-log correctness depends on immutable object design. DevStrap must create event
objects with unique keys and conditional put semantics (`If-None-Match: *` where the S3
client supports it), pull with bounded `ListObjectsV2` pagination and `next_cursor`, and
never append by overwriting one shared manifest object.

### A.3 Create S3-compatible credentials (least privilege)

1. **R2 → Manage R2 API Tokens → Create API Token.**
2. Permission: **Object Read & Write** (not Admin). Do **not** grant account-wide or
   bucket-management rights.
3. Scope: **Apply to specific buckets only → `devstrap-hub`**. In single-owner self-hosted
   mode this bucket-scoped token is acceptable. In hosted/SaaS or runner mode, the
   long-lived parent key must stay only in trusted control-plane code; devices and
   runners receive short-lived credentials or presigned URLs scoped to
   `workspaces/<workspace_id>/...` and the operations they need.
4. On creation Cloudflare shows three values **once**:
   - **Access Key ID**
   - **Secret Access Key** (shown only at creation — capture it immediately into the
     secrets path below; never paste it into a file)
   - the **S3 endpoint**, of the form `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`

Your **Account ID** is also shown on the R2 overview page. The S3 **region is `auto`** for
R2 (the SDK still requires a region string; use the literal `auto`).

### A.4 Configure DevStrap with R2

Connection settings vs. secrets are kept apart, mirroring `12_DATA_MODEL_SQLITE.md`
("Hub connection settings … are configuration, not schema, and never include plaintext
credentials in `state.db`"):

- **Non-secret connection settings** (bucket, endpoint, region, workspace prefix) are
  plain DevStrap config.
- **The Secret Access Key is a secret.** It goes through DevStrap's secrets path
  (**shipped, `P6-HUB-02`**), resolved most-explicit-first:
  1. `DEVSTRAP_HUB_S3_ACCESS_KEY_ID` / `DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY` env/config —
     either value may be a 1Password `op://vault/item/field` reference, resolved at sync
     time via `op read --no-newline` under the sanitized child environment (requires the
     `op` CLI on PATH and a signed-in account);
  2. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` literals (SDK-standard fallback);
  3. the per-workspace OS-keychain slot written once by `devstrap hub login`
     (account `hub-s3.<workspace_id>` under the device-identity keychain service; 0600
     file fallback when the keychain is genuinely unavailable, e.g.
     `DEVSTRAP_NO_KEYCHAIN=1`; removed with `devstrap hub logout`).
  Explicit env always overrides the stored pair (CI/12-factor). Plaintext env remains a
  sanctioned fallback (spec/13/spec/15 agree); the keychain or `op://` path is the
  recommended custody on developer boxes. A wrong or missing credential now surfaces as
  `ErrS3Auth` with a remediation hint, not an opaque `SignatureDoesNotMatch`; the age-blob
  custody variant was not built (keychain + op:// cover the client need). Server-side Fly
  secrets inject a plaintext env var at runtime and work as documented.

Client invocation (shipped, `P5-HUB-01`) — a developer box running sync:

```bash
# shipped (P5-HUB-01): the bucket is the r2:// URI host; --hub-file stays for tests only
devstrap sync   # hub: r2://devstrap-hub
```

Config / env names (shipped, `P5-HUB-01`). The bucket is the `r2://` (or `s3://`) URI host, not a separate env var. Non-secret settings:

```text
DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com  # shipped (or ?endpoint= on the URI)
DEVSTRAP_HUB_S3_REGION=auto                                          # shipped (default: auto)
```

Secret values — store them once with `devstrap hub login`, or supply env values (literal, or `op://` refs resolved at sync time); prefer an ephemeral export or direnv-ignored file over a shell profile when using plaintext env:

```text
DEVSTRAP_HUB_S3_ACCESS_KEY_ID                                        # shipped (id; low sensitivity, still not committed; AWS_ACCESS_KEY_ID fallback)
DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY                                    # shipped: literal or op:// ref (P6-HUB-02); keychain via `devstrap hub login`; AWS_SECRET_ACCESS_KEY fallback
```

Because R2's API is S3-compatible, the underlying client also honors the standard AWS SDK
names (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION=auto`,
`AWS_ENDPOINT_URL_S3=https://<ACCOUNT_ID>.r2.cloudflarestorage.com`). On the server side
these are injected from Fly secrets at runtime (section B). On a developer box the *target*
is to store the secret access key as a 1Password `op://` ref or an age-encrypted blob and
let DevStrap resolve it at sync time — the same machinery already used for `devstrap env` —
and that resolution is now **wired into the hub path** (`P6-HUB-02`, shipped): the client
resolves `op://` refs via `op read` and falls back to the `hub login` keychain slot before
constructing the S3 client; only a literal env/config value is passed through as-is.

DevStrap owns object lifecycle: blob **ref-counting** and garbage collection of unreferenced
`age_blob:<sha256>` objects happen client-side after device revoke/lost re-encryption
(`09_SECRETS_AND_ENVIRONMENT.md`), so no R2-side object-lifecycle rule is required.

> **Runbook caveat (`P6-HUB-01`): do not run `devstrap hub gc` against a live bucket until GC
> is sync-first and grace-windowed.** Today GC computes reachability from local state only;
> against a shared/real bucket that can delete blobs another device still references before
> this device has pulled the map. Pull the latest event log first and skip objects newer than
> a grace window before deleting. Prefer `--dry-run` until `P6-HUB-01` ships.

> **Runbook (`P4-HUB-11`): `devstrap hub compact` bounds event-log growth.** Concurrent
> destructive hub passes (`gc`/`compact`/`migrate-events`) on cooperating clients are serialized
> by an advisory sweep lock (`meta/sweep.lock`, `P4-HUB-12`): a real run acquires it after its
> pre-sync and before the destructive publish/delete, refusing with the holder id if another
> sweep is live and breaking a lock older than its 1h TTL. The lock protects cooperating clients
> only, not a hostile writer (`spec/15`). It converges first (pull+apply+push) and refuses from any incomplete replica
> (deferred/skipped/quarantined events, an open key-grant wait, or an open quarantine
> conflict), so it never deletes events another device still needs. Its order is
> confirm-before-delete — it publishes the sealed snapshot object and CAS-writes the signed
> retention manifest, reads the manifest back to confirm, and only THEN deletes the cold events
> below the floors — so a crash leaves a superset of the committed state. Floors are monotonic
> (it refuses to lower any device's floor). Use `--dry-run` to preview the floors and
> event-delete estimate without writing; `--keep-snapshots N` (default 2) bounds snapshot
> retention; `--min-events N` skips a compaction that would reclaim less than N events. A device
> that falls below a published floor recovers automatically on its next `devstrap sync` by
> importing the snapshot (`P4-SYNC-02`). Because the snapshot covers all cold segments, an
> R2-side object-lifecycle rule remains unnecessary — DevStrap deletes cold events itself only
> after the superseding snapshot is confirmed.

> **Runbook (`P4-HUB-12`): `devstrap hub migrate-events` once per pre-#59 hub.** A bucket that
> served the fleet before the per-device seq layout (PR #59) still holds events under the retired
> HLC-keyed `events/` prefix. `Pull` reads both layouts (dual-read), so nothing breaks, but the
> legacy prefix is listed on every pull until it is drained. Run `devstrap hub migrate-events`
> ONCE against such a bucket to re-key the legacy objects into `eventlog/<device>/` and delete
> the legacy prefix; afterward the dual-read freezes to a cheap empty-prefix list. It is
> idempotent, resumable, read-back-verified before each delete, and fails open (an object it
> cannot parse/decode/coordinate-match is kept, never deleted), so it is safe to re-run and safe
> to interrupt. Preview with `--dry-run`. It acquires the same advisory sweep lock as `gc`/
> `compact`. Buckets created at or after PR #59 never used the legacy layout, and the file-backed
> test hub never did either, so migrate-events is a no-op there.

### A.5 Cost note

R2's free tier currently includes 10 GB-month Standard storage, 1M Class A operations, and
10M Class B operations; paid Standard storage is $0.015/GB-month, Class A is $4.50/M, and
Class B is $0.36/M. **Egress is free**, which makes blob restore cheap, but polling can be
the first bill: `ListObjectsV2` is a Class A operation. Use cursor backoff, page limits,
event segments/snapshots, and avoid unbounded prefix scans. Standard storage is the
default for hot events/blobs; Infrequent Access has retrieval fees and a minimum storage
duration, so it is not appropriate for the hot event log. A card is required to enable R2
even while usage stays inside the free tier.

### A.6 Hub durability replica and restore drill (`P4-HUB-16`)

R2 confidentiality is not R2 durability. Cloudflare's current
[S3 compatibility table](https://developers.cloudflare.com/r2/api/s3/api/) was checked on
2026-07-14: `GetBucketVersioning` and `PutBucketVersioning` are unsupported.
`PutObjectLockConfiguration` and `GetObjectLockConfiguration` are also unsupported, and
Cloudflare's R2 changelog has stated that R2 does not yet support Object Lock. Therefore the
runbook is **not** “enable S3 versioning + Object Lock.” Use an independently-administered
replica hub so a primary bucket/account credential, deletion, corruption event, or ransomware
event does not also destroy the replicated namespace recovery artifact.

DevStrap reuses the normal multi-backend Hub interface. Configure `hub_replica` with a second
`r2://`, `s3://`, `git+ssh://`, `git@host:path`, `folder:`, or `file:` target. After every
successful `devstrap sync` (including the sync inside each `run-loop` tick), the client checks
`durability.export_interval` (default `24h`; `0` disables). When due it reads the signed
retention head from the primary, verifies the workspace, approved/local producer signature,
producer identity, and object SHA-256, then copies the exact immutable sealed snapshot named by that head
under the same `workspaces/<workspace_id>/snapshots/<sha256>.json` key on the replica, then
CAS-publishes the same signed retention manifest. An already-more-advanced same-workspace head
is a benign concurrent-exporter success; incomparable progress or a different workspace is
refused. Snapshot-before-manifest ordering leaves the
replica directly bootstrappable through the existing fail-closed snapshot-import path. If the
primary has no compaction snapshot, export prints a skip with the remedy to run
`devstrap hub compact`; it does not fail the otherwise-successful sync.

This export covers the **event-plane namespace snapshot and retention head only**. It does not
mirror the Hub blob plane: env-profile ciphertext and draft-bundle ciphertext remain separate
content-addressed `age_blob:<sha256>` objects, while the snapshot carries only their `BlobRef`
pointers. After total primary loss, the replica alone can restore namespace metadata (projects,
device trust, pointers, and other compacted state), but not the referenced env/draft content.
The RPO for that blob content is therefore whatever a surviving device still has cached locally
and can re-push, not the scheduled namespace-metadata RPO below. Full blob-plane replication is
future work; retain local copies on at least one surviving device for this runbook.

The namespace-metadata target RPO is **up to `export_interval` + normal sync lag, default
~24h**. That assumes the primary compaction snapshot is current: run `devstrap hub compact` at
least at the same cadence and after important namespace changes. Override the run-loop schedule for one invocation with
`--durability-export-interval <duration>`; the config key controls both one-shot `sync` and
`run-loop`. A successful copy is recorded locally in `local_meta`, keyed to the configured
replica URI, so changing the target forces an immediate first export.

Example: a primary bucket in Cloudflare account A and a replica bucket in separately-controlled
account B. Replica credentials are deliberately replica-scoped and do not fall back to the
primary `AWS_*` variables or primary keychain slot:

```yaml
# ~/.devstrap/config.yaml
hub: "r2://devstrap-hub-primary"
hub_replica: "r2://devstrap-hub-dr"
durability:
  export_interval: "24h"
```

```bash
# Primary credentials keep their existing DEVSTRAP_HUB_S3_* names.
export DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_A_ID>.r2.cloudflarestorage.com

# Replica credentials may be literal ephemeral env values or op:// references.
export DEVSTRAP_HUB_REPLICA_S3_ENDPOINT=https://<ACCOUNT_B_ID>.r2.cloudflarestorage.com
export DEVSTRAP_HUB_REPLICA_S3_REGION=auto
export DEVSTRAP_HUB_REPLICA_S3_ACCESS_KEY_ID='op://DevStrap/R2 DR/access-key-id'
export DEVSTRAP_HUB_REPLICA_S3_SECRET_ACCESS_KEY='op://DevStrap/R2 DR/secret-access-key'

devstrap hub compact
devstrap sync
devstrap doctor
```

For provider diversity, the replica can instead be a private git carrier:

```yaml
hub_replica: "git+ssh://git@backup.example.com/devstrap/dr.git?branch=main"
```

`devstrap doctor` reports `hub durability export` as OK when replication is unconfigured
(optional defense in depth) or fresh, warns when an opted-in replica has no successful export
or the last success is older than twice `export_interval`, and gives a sync/run-loop retry
remedy. A transient replica read/write outage is a loud warning during sync but does not halt
primary convergence; invalid replica configuration remains a hard failure. Doctor separately
queries open `event_hash_chain_break` conflicts. A break correlated to a pending workspace-key
grant and preserved undecryptable predecessor is an expected self-healing **warning**; only an
unexplained break is an **error** with “possible hub data loss, truncation, or corruption.”
Generic conflict listing remains unchanged.

Disaster-recovery drill (exercise this after provisioning and periodically thereafter):

1. Run `devstrap hub compact`, then `devstrap sync`; confirm `devstrap doctor` reports a fresh
   durability export.
2. From a surviving trusted device, temporarily make the replica URI the primary `hub:` value
   (retain the same workspace id, approved producer identity, and Workspace Content Key
   custody). Preserve the old primary setting outside the config before editing it.
3. Run `devstrap sync`. The replica's retention head forces the existing verified
   snapshot-bootstrap path when the local cursor is behind; compare `devstrap status` and
   representative project rows with the pre-drill state.
4. Restore a clean primary carrier from that trusted device, compact/sync, reinstate the normal
   primary and replica settings, then run `devstrap doctor` again. Do not delete the replica
   during the drill. A truly fresh device still needs the normal out-of-band producer pinning
   and WCK grant ceremony; the zero-knowledge replica cannot manufacture decryption keys.

This is different from `devstrap db backup --full` / `devstrap db restore`. Those commands
protect one local device's SQLite state, encrypted blobs, config, and key material. Durability
export protects the Hub's sealed namespace/event-plane compaction snapshot and recovery head
against primary bucket-level loss or corruption; it does not copy the blob plane described
above. Use both layers and preserve surviving-device blob caches.

---

## B. Fly.io — compute (control plane + agent runners)

Fly.io runs compute for **both** planes: a small Go control-plane API service as a long-lived app, and
**ephemeral per-task agent-runner Machines** (Firecracker microVMs). It is chosen for
microVM isolation, global regions (verify current choices with `fly platform regions`),
scale-to-zero / suspend-resume, and native execution of the Go binary. E2B (self-hostable microVM agent
sandboxes) is the documented runner escape-hatch (`03_SYSTEM_ARCHITECTURE.md`).

### B.1 Install flyctl and authenticate

```bash
brew install flyctl              # or: curl -L https://fly.io/install.sh | sh
fly version
fly auth signup                  # first time; or `fly auth login`
```

Add a payment method in the Fly dashboard billing page. Fly is pay-as-you-go; estimate
with current Fly pricing, including started/stopped Machine time, rootfs, volumes, IPs,
and outbound data transfer.

### B.2 Control-plane app

```bash
# from the control-plane service directory
fly launch              # detects the Go app, scaffolds fly.toml, picks an org + region
# or, to create explicitly without scaffolding:
fly apps create devstrap-hub-control
```

A minimal `fly.toml` for a Go HTTP service (illustrative — secrets are **never** in this
file; it is committed, secrets are not):

```toml
app = "devstrap-hub-control"
primary_region = "iad"            # pick a region near the operator / most devices

[build]

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = "suspend"  # scale-to-zero via suspend/resume
  auto_start_machines = true
  min_machines_running = 0

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
```

Set every secret with `fly secrets set` — values are stored encrypted by Fly, injected as
environment variables **at runtime only**, and never baked into the image:

```bash
fly secrets set \
  DEVSTRAP_HUB_S3_ACCESS_KEY_ID=...        \
  DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=...    \
  DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
  DATABASE_URL='postgres://...sslmode=require' \
  DEVSTRAP_SIGNING_KEY=...                  \
  --app devstrap-hub-control
```

`fly secrets set` triggers a rolling redeploy so the new values take effect. Use
`fly secrets list` to confirm names (it shows digests, never values). Rotate with another
`fly secrets set` and unset with `fly secrets unset`.

### B.3 Agent-runner Machines (ephemeral, per task)

Runners are **not** part of the long-lived control-plane app's autoscaling and should live
in a separate Fly app/org/process boundary. Each task gets its own Firecracker microVM
created on demand and destroyed when the task ends — **one Machine per task / tenant**:

```bash
# create a throwaway runner microVM for a single task
fly machine run <runner-image> \
  --app devstrap-hub-runners \
  --region iad \
  --rm \
  --env DEVSTRAP_AGENT_RUN_ID=<run-id>
```

For trusted single-owner runners, suspend/resume can keep warm capacity without paying for
idle CPU:

```bash
fly machine suspend <machine-id>
fly machine resume  <machine-id>
```

In production these are driven programmatically via the **Fly Machines REST API**
(`https://api.machines.dev/v1/apps/<app>/machines`) using a Fly access token held as a
control-plane secret, so the control plane spins runners up and tears them down per task.
Runners inherit secrets the same way — injected at boot, never in the image — and should
receive only the minimal scoped credentials a single task needs: no parent R2 key, no
bucket-wide key, no `DATABASE_URL` unless the task strictly requires it, TTL cleanup, no
shared volumes for untrusted tasks, and explicit egress/resource limits. For untrusted
tenant code, prefer destroy-after-task over suspend/resume because suspend preserves
memory state.

### B.4 Regions, scaling, one platform

Pick `primary_region` near the operator and the bulk of devices; add regions later by
scaling the app or launching runner Machines in other current Fly regions. Keep the
control plane and runner Machines in separate apps/process boundaries even if they share
an account/org, so secrets and blast radius stay separated. Multi-tenant scaling principles
(control/data-plane split, pooled→dedicated tenancy spectrum, cell-based scaling) are
recorded as future direction in `03_SYSTEM_ARCHITECTURE.md` and
`14_MVP_ROADMAP_AND_BACKLOG.md` (`SCALE-*`).

### B.5 Cost note

Pay-as-you-go. A small always-on control-plane Machine is a low single-digit dollars/month
order of magnitude, and auto-stop/suspend can reduce idle compute, but stopped Machines,
rootfs, volumes, IPs, and outbound transfer may still bill. Runner microVMs bill for their
short lifetime plus any storage/network they consume. There is no broad perpetual free
tier — budget a few dollars/month for a solo deployment and set budget alerts before SaaS.

---

## C. Neon — managed Postgres (control-plane database)

Neon is the **control-plane database** for the future multi-user/SaaS direction: accounts,
devices, billing, metering, and the tenant directory. It is **not** the sync data plane —
no code, secrets, drafts, or the namespace map live here; those are R2 + git. Neon is
chosen for serverless Postgres, scale-to-zero, and branchable databases for preview/test.

### C.1 Sign up and create the project

1. Sign up at `https://neon.tech` (Neon console).
2. **Create project** — choose the Postgres version and a region near the Fly control-plane
   app (co-locate to minimize latency). Neon provisions a default database and an owner role.

### C.2 Get the connection string(s)

The console's **Connection Details** panel gives the connection string. Note the two forms:

- **Pooled** — host carries a `-pooler` suffix; routes through PgBouncer. Use this for the
  control-plane service (many short-lived connections from Fly Machines).
- **Direct / unpooled** — no `-pooler` suffix. Use for migrations and admin tasks that need
  a session-level connection.

**SSL is required** — keep `sslmode=require` (newer Neon strings also include
`channel_binding=require`):

```text
postgres://<role>:<password>@<host>-pooler.<region>.aws.neon.tech/<db>?sslmode=require
```

### C.3 Create least-privilege runtime and migration roles

Do **not** ship the project owner role to the service. Use two DSNs and two roles:

- `devstrap_app` uses Neon's pooled connection string for runtime request handling.
- `devstrap_migrator` uses the direct/unpooled connection string for migrations, `pg_dump`,
  logical replication, and any session-level operation.

Create roles with only the privileges each path needs:

```sql
CREATE ROLE devstrap_app LOGIN PASSWORD '<generated>';
CREATE ROLE devstrap_migrator LOGIN PASSWORD '<generated>';
GRANT CONNECT ON DATABASE <db> TO devstrap_app;
GRANT CONNECT ON DATABASE <db> TO devstrap_migrator;
GRANT USAGE ON SCHEMA app TO devstrap_app;
GRANT USAGE, CREATE ON SCHEMA app TO devstrap_migrator;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA app TO devstrap_app;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA app TO devstrap_migrator;
ALTER DEFAULT PRIVILEGES IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO devstrap_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON TABLES TO devstrap_migrator;
-- no SUPERUSER, no CREATEROLE, no DDL on production schemas
```

Build the service's runtime `DATABASE_URL` from `devstrap_app`, not the owner. Keep the
direct migration/admin DSN separate and unavailable to normal request handlers.

### C.4 Configure DevStrap with Neon

Store the pooled application-role connection string as a **Fly secret** named
`DATABASE_URL` (section B.2) so it is injected at runtime and never committed or logged.
Store the direct migrator DSN separately, for example `DATABASE_MIGRATOR_URL`, and expose
it only to migration jobs. These are control-plane secrets and follow the same custody
rules as every other credential here.

### C.5 Branching and multi-tenancy

- **Branching** — Neon branches are copy-on-write database clones; use one per preview/CI
  environment so test runs never touch production data. A test Fly app simply points its
  `DATABASE_URL` at the branch's connection string.
- **Connection model** — Neon pooled connections run through PgBouncer transaction mode.
  Use them for runtime request handling, but use direct connections for migrations,
  `LISTEN/NOTIFY`, session advisory locks, prepared-statement-sensitive workloads, `pg_dump`,
  logical replication, and admin tasks.
- **Tenancy model** — for the eventual SaaS, choose between **schema-per-tenant**
  (stronger isolation, heavier migration fan-out) and **shared tables with a `tenant_id`
  column** (simpler operations, isolation enforced in app + row-level security). This is a
  control-plane choice only; the zero-knowledge data plane already isolates tenants by
  construction regardless (`15_SECURITY_THREAT_MODEL.md`, `SCALE-*`).

### C.6 Cost note

Neon's free tier currently covers a solo/preview control plane with scale-to-zero,
compute-hour allowance, and per-project storage limits. Launch/paid plans are usage-based
for CU-hours and storage and add higher limits, branches/history, and operational features
when the SaaS direction is built. Account for cold starts after scale-to-zero and keep
budget alerts on CU-hours, storage, branches, and egress.

---

## D. Cross-cutting

### D.1 Provisioning order & checklist

Provision dependencies before the thing that consumes them — storage and database first,
then the compute that holds their credentials:

```text
1. R2:   create bucket + scoped API token        -> Account ID, Access Key ID, Secret Access Key, endpoint
2. Neon: create project + least-priv app role     -> DATABASE_URL (pooled, sslmode=require)
3. Fly:  create app + `fly secrets set` (R2 keys, DATABASE_URL, signing key)
4. Fly:  deploy control plane; wire runner Machines via the Machines API
```

Checklist:

- [ ] Cloudflare account created; R2 enabled; `devstrap-hub` bucket created.
- [ ] R2 **Object Read & Write** token scoped to `devstrap-hub` only; Account ID + keys + endpoint captured into Fly secrets (server) and a non-committed env source (client, pending `P6-HUB-02`).
- [ ] Neon project created; least-privilege `devstrap_app` role created (not the owner).
- [ ] `DATABASE_URL` built from the app role with `sslmode=require`.
- [ ] flyctl installed; authenticated; payment added.
- [ ] Control-plane app created; `fly secrets set` holds R2 keys, `DATABASE_URL`, signing key.
- [ ] `fly.toml` committed **without** secrets; image builds; control plane deploys.
- [ ] Runner Machines provision/destroy per task via the Machines API.
- [ ] No plaintext credential is in any committed file, shell profile, or log.

### D.2 Credential custody rules

- **Two custody locations, one rule — never plaintext.**
  - *Server side* (Fly control plane + runners): all secrets live in **Fly secrets**,
    injected at runtime, never baked into the image, never in `fly.toml`.
  - *Client side* (a developer box running `devstrap sync`): the **target** is DevStrap's
    **existing encrypted secrets path** — OS keychain / Secret Service, an age-encrypted
    `age_blob:<sha256>` blob under `~/.devstrap/blobs`, or a 1Password `op://` ref resolved
    at use time. **Not yet true for the hub S3 secret (`P6-HUB-02`):** the hub path currently
    reads it as a plaintext env var / config line only. Non-secret connection settings
    (bucket, endpoint, region, workspace prefix) may live in plain config.
- **Never** commit a secret to git, write it to a logged file, or echo it; DevStrap's
  value-level redaction (`internal/redact`) already scrubs secret-shaped values from logs
  and errors — do not defeat it by printing raw credentials.
- **Rotate on exposure.** Cloudflare: roll the R2 API token. Fly: `fly secrets set` the new
  value (rolling redeploy) and `fly secrets unset` the old. Neon: rotate the app role
  password and rebuild `DATABASE_URL`. Device revoke/lost additionally re-encrypts affected
  blobs and flags exposed values for rotation (`09_SECRETS_AND_ENVIRONMENT.md`).
- **Least privilege everywhere:** R2 token = one bucket, read/write objects only; Neon role
  = app grants only, never owner/superuser; runner Machines get only the scoped credentials
  a single task needs.

### D.3 Platform → DevStrap role

| Platform        | DevStrap role                                | Holds                                         | Credential custody                         |
| --------------- | -------------------------------------------- | --------------------------------------------- | ------------------------------------------ |
| Cloudflare R2   | Hub **data plane** (zero-knowledge)          | Signed event log + `age_blob:<sha256>` ciphertext | Fly secret (server, shipped); client target: keychain / age blob / `op://` — today plaintext env only (`P6-HUB-02`) |
| Neon (Postgres) | **Control-plane DB**                         | Accounts, devices, billing, metering, tenants | `DATABASE_URL` as a Fly secret             |
| Fly.io          | **Compute** for both planes + runner microVMs | Control-plane API; ephemeral per-task runners | All secrets via `fly secrets`              |

### D.4 Cost / free-tier summary

| Platform        | Free tier                                             | Notes                                          |
| --------------- | ----------------------------------------------------- | ---------------------------------------------- |
| Cloudflare R2   | 10 GB-month Standard storage, 1M Class A ops, 10M Class B ops; **zero egress** | Standard: $0.015/GB-month, Class A $4.50/M, Class B $0.36/M; polling/listing can dominate |
| Fly.io          | None broad/perpetual for new deployments (pay-as-you-go) | Estimate Machines, rootfs, volumes, IPs, and egress; runners bill per task |
| Neon (Postgres) | Free plan with scale-to-zero, CU-hour allowance, and per-project storage/egress limits | Paid Launch/Scale add CU/storage/branch/history capacity; use pooled runtime + direct migration DSNs |

### D.5 Provider alternatives and decision

The audited recommendation is **keep Fly.io + Cloudflare R2 + Neon** as the documented
direction. It fits DevStrap's Go-first binary, low-idle personal fleet, encrypted
object-store hub, and future per-task runner isolation.

- **Tigris** is the closest R2 alternative because it is Fly-native, S3-compatible, has
  zero egress, and offers global data placement. Prefer it if one-vendor Fly integration
  and placement behavior matter more than R2's lower Standard storage/operation pricing
  and larger free tier.
- **Cloudflare Workers + Durable Objects/D1 + R2** is credible for a future serverless
  HTTP/SSE/control edge, especially if live push becomes important. It is not the primary
  plan because DevStrap is Go-first and direct R2 avoids operating a service for the
  single-owner fleet.
- **Supabase** is attractive if DevStrap needs Auth/Storage/BaaS in addition to Postgres.
  It is less ideal as the default simple control DB when Neon scale-to-zero and branching
  are enough.
- **Render/Railway** are simpler app-hosting options for trusted deployments, but they do
  not replace microVM-style runner isolation for untrusted multi-tenant code.
- **Hetzner/self-hosted** remains the cheapest always-on solo option but lacks the managed
  global/scale-to-zero/microVM runner story.

### D.6 Cross-references

- `03_SYSTEM_ARCHITECTURE.md` — Hub backend interface; the *Hosting & scaling (FUTURE)*
  subsection that selects Fly.io + R2 + managed Postgres (`SCALE-*`); the planned HTTP/SSE
  wire protocol.
- `07_NAMESPACE_AND_SYNC_MODEL.md` — the namespace map / event log, HLC ordering, and the
  R2 (Option C, object-store) backend behind the pluggable `Hub` interface (`HUB-*`).
- `09_SECRETS_AND_ENVIRONMENT.md` — the encrypted secrets path, `age_blob:<sha256>` blobs,
  `op://` refs, device recipients, and blob ref-counting / GC.
- `12_DATA_MODEL_SQLITE.md` — R2 per-workspace key prefixing and the rule that hub
  connection settings are config, not schema, with no plaintext credentials in `state.db`.
- `13_CLI_DAEMON_API.md` — the shipped `hub: r2://<bucket>` hub config value (and `--hub-file` test-only backend).
- `15_SECURITY_THREAT_MODEL.md` — the two-plane zero-knowledge hub trust model and
  confidentiality-by-construction caveat (`HUB-*`, `SCALE-*`).
- `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` — the `HUB-*` (cloud zero-knowledge hub on R2) and
  `SCALE-*` (multi-user hosting on Fly.io + R2 + managed Postgres) workstreams that drive
  this guide.

---

## E. Pair a second device

This is the end-to-end two-device pairing runbook: a **two-paste ceremony** (founder code →
joiner, joiner code → founder). Since `P7-PROD-01` (slice 1) the joiner side is a single
`devstrap join <code>` command, and the founder's fingerprint travels **inside** the code, so
the out-of-band fingerprint read is now **optional high-assurance** (below) rather than
mandatory in each direction. It closes the local pairing plane of `P4-SEC-04` (founder-pinning +
full-256-bit fingerprint confirmation) and `P4-SEC-07` (workspace-id adoption); founder-side
*automation* (the `devstrap pair`/`devstrap up` wizard) remains future work (slice 2).

The R2/S3 hub keys every object under `workspaces/<workspace_id>/` (section A.2), so two
devices converge only when they share one workspace id. The **founder** mints it at
`devstrap init`; every later device **adopts** it — in the ceremony below, from the founder's
one-paste `devstrap-pair2:` code (`devstrap join <code>`, which folds `init --join --code` +
`hub init`). A device that runs a bare `devstrap init` mints its own fresh id, keys a disjoint
prefix, and never sees the founder's content — joining is a first-run decision, not something
you can retrofit (see *E.7 Not supported*).

The workspace id is a **non-secret prefix selector**, not a credential: it is excluded from
event signatures by design and re-stamped empty on apply (`15_SECURITY_THREAT_MODEL.md`).
Confidentiality and authorization come from the **key exchange**, not the id — you exchange
the id out-of-band alongside the founder's public keys, Syncthing-style, and each side
authorizes the other by pinning its verified keys. A wrong id only ever yields an empty prefix
or quarantined ciphertext, never someone else's plaintext.

> **Blobs are unauthenticated by design — even the embedded fingerprint.** The `devstrap-pair2:`
> code carries the workspace id, device id, display name, OS, arch, age recipient, signing public
> key, and (new in `P7-PROD-01`) the founder's fingerprint plus an optional hub URI — but still
> no MAC and no signature. The embedded fingerprint is a **convenience and a corruption check**,
> NOT authentication: `Decode` derives the fingerprint from the carried KEYS and refuses if the
> embedded value disagrees (catching a mangled paste), but an attacker who rewrites the blob in
> transit regenerates a self-consistent fingerprint for their substituted keys just as the
> legitimate sender did. So `devstrap join`'s default (auto-trust the embedded fingerprint) trusts
> your **paste channel**; the only defense against a *compromised* channel is still the
> out-of-band read-aloud — pass `devstrap join --fingerprint <fp>` (or the founder's derived value
> to `devices approve`/`enroll`) to enforce it. Non-key fields (name/os/arch/hub) are not
> fingerprint-bound — tampering with them cannot forge trust, but it can break convergence
> **visibly** (quarantined events, or a `doctor`-detected wrong hub prefix) until you re-run the
> ceremony with a fresh code. A v1 `devstrap-pair1:` code (older binaries) carries no fingerprint
> and always requires the out-of-band read. `Decode` still parses it exactly.

### E.1 Founder — found the workspace and publish the pairing material

```bash
devstrap init ~/Code                 # mints the workspace id; does NOT self-mint a WCK yet
# point sync at the hub in ~/.devstrap/config.yaml — the zero-infra default is an empty
# private git repo (`hub: "git@github.com:you/devstrap-hub.git"`, no login needed).
# For R2/S3 use `hub: r2://<bucket>` plus DEVSTRAP_HUB_S3_ENDPOINT (or `?endpoint=`)
# and run `devstrap hub login` to store the secret in the keychain slot — see A.4/B and E.3.
devstrap sync                        # founds epoch 1 against the empty hub and pushes the namespace map
devstrap status                      # the `Workspace ID:` line (also `--json` → workspace_id)

devstrap devices pairing-code        # stdout: devstrap-pair2:... (fingerprint + hub embedded)  stderr: fingerprint + next steps
```

Share the **non-secret** `devstrap-pair2:` blob (stdout) with the second device by any channel.
The founder **fingerprint** still prints to stderr — read it aloud only if you want the
high-assurance out-of-band check (the joiner passes it to `devstrap join --fingerprint`);
otherwise the embedded value carries it. `devstrap devices recipient --fingerprint` prints the
same fingerprint if you need it again for a script or a check.

### E.2 Joiner — join in one command

`devstrap join <code>` folds the whole joiner side — adopt the workspace id, pin the founder,
configure the hub from the embedded URI, and print this device's own code to send back:

```bash
devstrap join '<founder-code>'
```

By default it **auto-trusts the embedded fingerprint** (no prompt) — this trusts your paste
channel, it is not cryptographic authentication. For the high-assurance check, pass the founder
fingerprint you confirmed out-of-band; a mismatch fails **before any filesystem write**:

```bash
devstrap join '<founder-code>' --fingerprint <founder-fingerprint>
```

If the founder's code carried a hub URI, `join` writes it into `~/.devstrap/config.yaml`
automatically (skip E.3's `hub init`); if it carried none, `join` says so and you run
`devstrap hub init <url>` yourself before the first sync. On a **v1** code (older founder binary,
no embedded fingerprint) `join` falls back to the same interactive/`--fingerprint`/pending
behavior `init --join --code` has always had.

The manual, step-by-step fallback is still supported for recovery, tests, and v1 codes:
`devstrap init ~/Code --join --code '<founder-code>' --fingerprint <fp>` (adopt + pin), then
`devstrap hub init <url>` (E.3), then `devstrap devices pairing-code`. `--code` implies `--join`,
adopts the founder's workspace id, and enrolls + pins the founder row in one command; without
`--fingerprint` a TTY prompts for `yes` and a non-TTY stores the founder **pending** with the
exact `devstrap devices approve <founder-device-id> --fingerprint <derived-fp>` follow-up to run
before the first sync. The code-free variant (`init --join --workspace-id <id>` then a full
`devstrap devices enroll <founder-device-id> … --approve --fingerprint <fp>`) is documented in
`13_CLI_DAEMON_API.md` (`init`, `join`, `devices enroll`). A bare `devstrap init --join` with
neither `--code` nor `--workspace-id` still initializes but warns that r2/s3 hubs key by
workspace id and will not converge until you adopt one.

<!-- MD028 separator between adjacent blockquotes -->

> **Fleets larger than two devices:** pinning any one device flips verification fail-closed,
> and device records are not synced — so pin **every** existing device this way, not just the
> founder. Events signed by a device you have not pinned yet quarantine as visible
> `event_verification_failure` conflicts and replay automatically once you enroll + approve
> that device (`conflicts list` shows them; nothing is lost).

### E.3 Joiner — log in to the hub (order matters; R2/S3 only)

`devstrap join` already wrote the hub config and printed this device's own code, so for a git
carrier E.3 is a no-op. For R2/S3 you still supply the credential pair:

```bash
# the hub config is already set (join wrote it, or the founder used a git carrier that needs
# no login); for R2/S3 (`hub: r2://<bucket>` plus DEVSTRAP_HUB_S3_ENDPOINT) store the secret:
devstrap hub login   # R2/S3 only: store the secret — AFTER the id-adopting join/init in E.2

# only if you used the manual init fallback (join already printed this):
devstrap devices pairing-code   # the joiner's own code + fingerprint, to send back to the founder
```

> **Keychain ordering trap.** The hub S3 credential slot is keyed on the workspace id
> (`hub-s3.<workspace_id>`). Run the id-adopting `devstrap join …` (or `init --join --code …` /
> `--workspace-id …`) **first**, then `hub login`. If you `hub login` before adopting the id (or
> re-initialize under a different id afterward), the credential lands in the wrong slot and is
> orphaned — just `hub login` again under the adopted id.

Send the joiner's `devstrap-pair2:` code (from `join`, or `devices pairing-code`) to the founder;
read its fingerprint aloud only for the high-assurance check.

### E.4 Founder — approve the joiner, then both sync

The founder enrolls and approves the joiner in one command — `--approve` wraps every held WCK
epoch to the joiner's recipient (`GrantAllEpochs`), so the joiner can decrypt the full history:

```bash
# on the founder
devstrap devices enroll --code '<joiner-code>' --approve --fingerprint <joiner-fingerprint>
devstrap sync        # pushes the device.key.granted events

# on the joiner
devstrap sync        # ingests the verified grants, decrypts the map, and materializes the tree
```

After this last `sync` the whole `~/Code` tree is really present on the joiner: every repo
blobless-cloned from its remote, draft blobs extracted, env profiles hydrated.

### E.5 Rotation cadence — keep the workspace key fresh

The **workspace content key (WCK)** that seals the namespace-map event log rotates on a
periodic cadence, independent of pairing:

- **Automatic during `sync`.** Any device whose active WCK epoch is older than
  `keys.rotate_max_age` (**default 90 days**; `0` disables) mints the next epoch during a sync,
  grants it to every approved device it knows, and publishes it. The check runs *after* the
  pull, so a freshly-ingested grant resets the local age and at most one device in a fleet
  rotates per deadline instead of everyone storming it. Override for one run with
  `devstrap sync --key-max-age 720h` (or `0` to skip).
- **Manual.** `devstrap keys rotate` mints and grants a fresh epoch on demand.
- **Forward-exposure only.** Rotation is a pure key-roll: it bounds how long a *silently*
  compromised key keeps reading new traffic, and deliberately does **none** of revoke's
  containment — no secret-rotation flags, no blob re-encryption, no hub deletes. Everything
  already sealed under the old epoch stays readable to whoever holds it.
- **For a known compromise, revoke.** `devstrap devices revoke <id>` rotates the WCK *and*
  re-encrypts affected blobs to the reduced recipient set and flags exposed secrets for
  value rotation (`09_SECRETS_AND_ENVIRONMENT.md`). Rotation is the routine hygiene; revoke is
  the incident response.

Doctor rows to watch: **`workspace key age`** (warns once the active epoch is past
`keys.rotate_max_age`, with the `keys rotate` remedy) and **`awaiting key grants`** (lists any
epoch/kid this device has seen ciphertext for but never been granted — see *E.6*).

### E.6 Wedge recovery — a device stuck "awaiting key grants"

**Symptom.** After a rotation happened while a device was *unknown to the rotator* (the device
registry is per-device, so a rotator only grants the approved devices it knows locally), that
device can see `doctor`'s **`awaiting key grants`** warning and undecryptable rows in
`conflicts list`. Its sync does not wedge — the missing-key defer is grace-bounded
(`sync.key_grant_grace`, default 72h) and past the window the still-encrypted carriers
quarantine as replay-recoverable `undecryptable` conflicts while the cursor advances — but the
affected events stay quarantined until the grant arrives.

**Recovery.** Re-grant from a device that holds **all** epochs:

```bash
# on a complete device (one that already holds every WCK epoch)
devstrap devices approve <stuck-device-id> --fingerprint <stuck-device-fingerprint>
devstrap sync        # pushes the catch-up device.key.granted events

# on the stuck device
devstrap sync        # ingests the grants; quarantined events replay and resolve automatically
```

Approving re-runs `GrantAllEpochs`, so the stuck device receives every epoch it was missing;
the per-pull replay (`ReplayUndecryptableConflicts`) then decrypts and applies the quarantined
carriers through the normal verified path and clears the conflicts — nothing is lost.

**`--allow-epoch-gap` override.** `devices approve` / `enroll --approve` refuse *before* any
trust write when the **approver's own** keyring is incomplete (a gap in held epochs `1..max`,
or an open `awaiting key grants` row), because the grant set would inherit the gap and strand
the approved device. `--allow-epoch-gap` forces the approval anyway; the approved device then
quarantines events at the missing epochs until it is re-approved from a complete device. While
that gap lasts, the gap device's open quarantine conflicts also keep **`devstrap hub gc`
refused** on it — run GC from a complete device, or close the gap first.

### E.7 Not supported — changing the workspace id on an initialized store

There is **no** in-place rewrite of the workspace id. `init --join --code/--workspace-id`
refuses if the store was already initialized under a different id (born-correct or not at all),
because a post-hoc rewrite would strand every object already keyed under the old prefix and
orphan the `hub login` credential slot (E.3). The only supported remedy is to discard the local
store and re-join cleanly:

```text
remove <home> and re-run: devstrap init ~/Code --join --code '<founder-code>' --fingerprint <fp>
```

where `<home>` is the DevStrap home (`~/.devstrap` by default). This is safe: no repo content
lives there — repos re-clone from their remotes and env/draft blobs re-pull from the hub on the
next `sync`.

## F. Live dogfood validation log

Chronological record of live dogfood runs against real hub backends, simulating multiple
devices on one Mac via per-device `--home`/`--root` + `DEVSTRAP_NO_KEYCHAIN=1`. R2 runs are
driven from the `~/.devstrap/dogfood-r2.env` creds file (see `AGENTS.md` § *Live-R2 dogfood
credentials*); git-carrier runs need no creds file at all — auth is the machine's existing
ssh key.

### F.1 Compact + snapshot bootstrap (2026-07-04) — **PASS**

First live exercise of the snapshot-exchange wave (`hub compact` + fresh-device snapshot bootstrap,
`P4-SYNC-02`/`P4-HUB-11`). Three simulated devices, fresh workspace (own R2 prefix). All clean:

1. **A (founder)** `init` → `db migrate` → `add` 3 repos → `sync` = *pushed 3*, minted WCK epoch 1.
2. **B (join)** `init --join --code <A> --fingerprint <A-fp>` (adopts A's workspace id + pins A) →
   A `devices enroll --code <B> --approve --fingerprint <B-fp>` → A `sync` (grant) → B `sync`
   materialized 3/3. The `--fingerprint` flag on both sides makes the one-paste ceremony scriptable
   (skips the interactive compare-and-confirm).
3. Churned to **6 repos** across A+B, converged both (final syncs *push/pull 0* — exact per-device Seq boundary).
4. **`hub compact` on A** (a complete replica): `--dry-run` reported "would delete ~7 cold events, publish
   a snapshot of 6 entries / 2 anchors, keep 2 snapshots"; the real run: *"published snapshot 5f144f0efc44;
   advanced 2 device floor(s); deleted 7 cold event(s)"* — **the event log is bounded**. Floors are per-device
   Seq (dev_A=7, dev_B=2 — each device's own consumed watermark).
5. **Fresh device C** (`init --join` → approve → grant; pull cursor 0, below the floor): `sync` printed
   **"Recovering from hub snapshot (retention floor passed our cursor)…"**, imported the sealed snapshot,
   and **materialized 6/6 projects** — converging to the full namespace despite the 7 cold events being gone.
6. Incumbents A/B synced post-compact with **no** false "Recovering" (cursors above the floor); `hub gc`
   clean; a 2nd compact was a no-op; C `doctor --remote` = 24 ok / 0 errors.

Trap re-confirmed: run `db migrate` on each device home before its first `sync` (sync does not auto-migrate).
Earlier same-day/prior runs (pairing ceremony, `keys rotate`, per-device Seq cursor migration + late-push
delivery) are recorded in `spec/18_WORK_LOG.md` and the project memory.

### F.2 Git carrier against a real private GitHub repo (2026-07-04) — **PASS**

First live exercise of the AD-1 zero-infrastructure git carrier against a real forge:
`hub: "git@github.com:<owner>/devstrap-hub-dogfood.git"` (a fresh, **empty** private GitHub repo —
`gh repo create --private`, no README, no branch protection). Three simulated devices, real private
project repos, **no creds file and no `hub login` anywhere** — the zero-infra payoff held end-to-end:

1. **A (founder)** `init` → `db migrate` → `add` 2 private repos → `sync` = *"pushed 2 …
   materialized 2/2"*; `git ls-remote` on the carrier showed `refs/heads/main` created by the first push.
2. **B (join)** one-paste ceremony with `--fingerprint` on both sides → B `sync` = *"pulled 3;
   materialized 2/2"* — real blobless clones of private repos over GitHub ssh. Churned to **6 projects**
   across A+B; converged (final syncs *push/pull 0*).
3. **Concurrent push race:** one local event staged on each device, `sync` launched simultaneously on
   both — **both exited 0** and each reported *"pushed 1"* (the non-fast-forward refetch-and-reapply
   loop resolved the ref collision); the next syncs delivered both events. 8 projects total, converged.
4. **Ciphertext spot-check:** a plain `git clone` of the carrier contains only `devstrap-hub.json`,
   `workspaces/<ws>/**`, and `.devstrap-meta/times/**`; grepping the checkout for project names/paths
   found **no plaintext** (event envelopes expose ids/HLC by design — spec/15: host sees metadata,
   contents are ciphertext).
5. **Clean auth failure:** with the hub pointed at an inaccessible repo, `sync` failed in **1s**,
   fully non-interactive (BatchMode; no prompt, no hang), exit 6 (`exitAuth`), quoting git's
   *"ERROR: Repository not found."* — correct classification, though no `ssh-add`-style remedy hint
   is printed for this failure class (polish follow-up in `spec/18`).
6. **`hub compact` on A:** `--dry-run` reported "would publish a snapshot of 8 entries / 2 anchors,
   delete ~9 cold events, keep 2 snapshots"; the real run: *"published snapshot 2abfbbb7983e; advanced
   2 device floor(s); deleted 9 cold event(s)"*. Remote history bounded **18 commits → 2**:
   the parentless *"devstrap hub compact"* squash root plus the *"devstrap hub sweep-unlock"* release
   commit (the hermetic `rev-list --count == 1` assertion measures immediately after
   `CompactEventsBelow`, before the unlock batch lands).
7. **Fresh device C** (join post-compact, cursor 0 below the floor): `sync` printed **"Recovering from
   hub snapshot (retention floor passed our cursor)…"** and **materialized 8/8 projects**.
8. Incumbents A/B synced post-compact with **no** false "Recovering"; `hub gc` clean; a 2nd compact
   `--dry-run` ≈ no-op; C `doctor --remote` = **25 ok / 0 errors** with hub reachable `git:<ws>` —
   but **no** `workspace id match` probe row: `isRemoteHubID` (`internal/cli/doctor.go`) matches only
   `r2:`/`s3:`, so the git carrier skips the joiner never-pulled heuristic (fix in flight).
9. **Real-remote conformance:** `DEVSTRAP_HUB_GIT_TEST_REMOTE=git@github.com:<owner>/devstrap-hub-conformance.git
   go test ./internal/hub -run TestGitCarrierRealRemoteConformance` — **PASS (26s)** against a second
   disposable private repo.

Traps: `db migrate` before each device's first `sync` (re-confirmed). On a Mac whose ssh key is
keychain-loaded, even agent-less shells authenticate — an "unloaded key" is hard to simulate locally;
test the failure path with an inaccessible repo URL instead. A non-TTY `init --join` without
`--fingerprint` leaves the founder unpinned and prints the exact `devices approve … --fingerprint`
remedy — behaved as designed; pass `--fingerprint` to keep the ceremony scriptable.

### F.3 Multi-device completeness wave — env exchange + trust propagation (2026-07-05) — **PASS**

First live exercise of the ENV-SYNC-01 + TRUST-01 wave (PRs #130–#132) on the R2 hub, run BEFORE the
TRUST-01 merge from the feature binary — and it **caught a real bug** (see 5). Two fresh workspaces on
the shared `devstrap-hub` bucket (prefix isolation), three simulated devices (`--home`/`--root` +
`DEVSTRAP_NO_KEYCHAIN=1`), creds via `~/.devstrap/dogfood-r2.env`, one real private project repo.

1. **Pairing before capture (ordering matters):** A founds, adds the repo, first `sync` mints WCK
   epoch 1; B and C join via the one-paste `--fingerprint` ceremony and are approved BEFORE any env
   capture — env blobs are age-encrypted to the recipient set **at capture time**, so a device
   enrolled later cannot decrypt an existing blob without a re-capture/rotate.
2. **ENV-SYNC-01:** A `env capture` = *"2 env variables … for 3 recipient device(s)"* → `sync`
   pushed the `env.profile.updated` event + blob; B `sync` then `env hydrate --write .env.local`
   reproduced A's values **byte-identical**. No extra commands beyond the killer loop.
3. **TRUST-01:** A `devices revoke <B>` (epoch 1→2, superseding env rewrap, trust event in the same
   tx) → A `sync` → C `sync`: C's `devices list` showed B **revoked with no local operator action**
   — the headline. B's next pull deferred *"awaiting workspace key grant"* on epoch 2 (the designed
   wedge-out); B could still push a rogue project, which C pulled and **quarantined** (project absent
   from C's status, one loud `event_verification_failure` conflict).
4. **Rotation propagation (fixed-binary re-run, second workspace):** after the fix in 5, A's doctor
   showed *"warning secrets needing rotation 1"*, C's doctor showed the SAME warning purely via sync,
   and C's hydrate still worked through the superseded (rewrapped) blob ref.
5. **Bug caught:** the rewrap's superseding `env.profile.updated` re-inserted `secret_bindings` rows
   with `needs_rotation` cleared — wiping the revoke's rotation flags on the revoker AND every peer
   (P5-PROD-03 doctor surfacing silently broken). The txtar's `stdout 'rotation'` assertion was too
   weak to catch it (it matched the label in *"rotation 0"*). Fixed in PR #132: the upsert carries
   each var's flag forward; the txtar now asserts `secrets needing rotation [1-9]`.

Traps: the git carrier is **workspace-bound** by its `devstrap-hub.json` marker — a second workspace
pointed at `devstrap-hub-dogfood` is refused (create a fresh empty repo per workspace, or use R2,
whose per-workspace prefixes isolate); `hub init` bootstraps only git carriers — for r2/s3 set `hub:`
in config.yaml or export `DEVSTRAP_HUB` per shell. Assert doctor COUNTS (`[1-9]`), never bare labels.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-HUB-02 — Hub S3 credential custody contradicts this guide (RESOLVED 2026-07-03, `fix/p6-hub-02`)

**Problem (was).** `selectBackendHub` (`internal/cli/hub.go:106-110`) reads the secret literally
from `hub_s3_secret_access_key` / `AWS_SECRET_ACCESS_KEY` and passes it straight to
`NewS3Client` (`internal/hub/s3client_awssdk.go:60-67`). The keychain / age-blob / `op://`
resolution this guide promised (and annotated "shipped") does not exist, so a `op://` value
is signed literally and fails with an opaque `SignatureDoesNotMatch`. spec/13:182 and
spec/15:138 correctly document plaintext-env custody, so the three specs disagree.

**Actionable steps.**
1. Implement resolution in `selectBackendHub`: `op://` via the existing 1Password path (as in `env.go`), else the OS keychain via `devicekeys.NewHybridStore` (with a `devstrap hub login` / `env bind`-style command to store it once), keeping the plaintext-env fallback behind `DEVSTRAP_NO_KEYCHAIN` for CI.
2. Wrap the resolved value in `redact.Secret`; add an auth-error branch to `mapS3Error` with an actionable hint instead of a bare `SignatureDoesNotMatch`.
3. Reconcile spec/19 ↔ spec/13/spec/15 and drop the false "shipped" annotation on the secret custody until the feature lands (done in this pass).

**Example.**

```go
// selectBackendHub, before NewS3Client — resolve, don't pass through literally.
secret, err := resolveHubSecret(ctx, v.GetString("hub_s3_secret_access_key")) // op:// | keychain | plaintext(DEVSTRAP_NO_KEYCHAIN)
if err != nil {
    return nil, fmt.Errorf("resolve hub S3 secret: %w", err)
}
client, err := hub.NewS3Client(ctx, hub.S3Config{
    AccessKeyID:     accessKeyID,
    SecretAccessKey: redact.Secret(secret), // never logged
    Endpoint:        endpoint,
    Region:          region,
})
```
