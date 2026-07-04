---
last_reviewed: 2026-07-03
tracks_code: [internal/hub/**, internal/cli/hub.go]
---
# Cloud Provisioning & Configuration Guide

> **Status: the single-owner R2/S3 recipe is shipped (`P5-HUB-01`); the multi-tenant
> SaaS direction (`SCALE-*`, Fly.io compute, managed Postgres control plane) remains
> FUTURE.** This guide is the provisioning runbook for both: it doubles as a single-owner
> deployment recipe usable today and the SaaS hosting direction for later. The shipped sync
> transport is `devstrap sync` with `hub: r2://<bucket>` (the `aws-sdk-go-v2` S3 adapter;
> `--hub-file <path>` stays for tests). Env/config keys below marked *shipped* are live;
> those marked *planned* name the intended SaaS surface and are provisional until `SCALE-*`
> ships. See `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` (decisions 5 and 6) and
> `14_MVP_ROADMAP_AND_BACKLOG.md`.

> **Direction — zero-infrastructure quickstart (AD-1, planned).** Provisioning an R2 bucket
> is real first-run friction and undercuts the "new machine in minutes" promise. Because the
> hub only ever holds ciphertext plus signed events, even a "dumb" carrier is a safe
> zero-knowledge boundary. DIRECTION: add a **zero-infrastructure Hub backend behind the
> existing pluggable `Hub` interface** — a private-git-repo-backed and/or
> local-folder / cloud-drive-folder backend — and make it the quickstart default, keeping
> `hub: r2://<bucket>` (this guide) as the scale/power option. This guide then becomes the
> *power-user / self-hosting* path rather than a precondition for first sync. Not built yet;
> tracked with the adoption workstream in `14_MVP_ROADMAP_AND_BACKLOG.md`.

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

> **Runbook (`P4-HUB-11`): `devstrap hub compact` bounds event-log growth.** Run it from ONE
> designated device (concurrent compactions are not yet coordinated — the sweep lock is a
> follow-up). It converges first (pull+apply+push) and refuses from any incomplete replica
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

### A.5 Cost note

R2's free tier currently includes 10 GB-month Standard storage, 1M Class A operations, and
10M Class B operations; paid Standard storage is $0.015/GB-month, Class A is $4.50/M, and
Class B is $0.36/M. **Egress is free**, which makes blob restore cheap, but polling can be
the first bill: `ListObjectsV2` is a Class A operation. Use cursor backoff, page limits,
event segments/snapshots, and avoid unbounded prefix scans. Standard storage is the
default for hot events/blobs; Infrequent Access has retrieval fees and a minimum storage
duration, so it is not appropriate for the hot event log. A card is required to enable R2
even while usage stays inside the free tier.

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
joiner, joiner code → founder) plus one out-of-band fingerprint read in each direction. It
closes the local pairing plane of `P4-SEC-04` (founder-pinning + full-256-bit fingerprint
confirmation) and `P4-SEC-07` (workspace-id adoption); founder-side *automation* and an
in-band fingerprint UX remain future work.

The R2/S3 hub keys every object under `workspaces/<workspace_id>/` (section A.2), so two
devices converge only when they share one workspace id. The **founder** mints it at
`devstrap init`; every later device **adopts** it — in the ceremony below, from the founder's
one-paste `devstrap-pair1:` code (`devstrap init --join --code <code>`). A device that runs a
bare `devstrap init` mints its own fresh id, keys a disjoint prefix, and never sees the
founder's content — joining is a first-run decision, not something you can retrofit (see *E.7
Not supported*).

The workspace id is a **non-secret prefix selector**, not a credential: it is excluded from
event signatures by design and re-stamped empty on apply (`15_SECURITY_THREAT_MODEL.md`).
Confidentiality and authorization come from the **key exchange**, not the id — you exchange
the id out-of-band alongside the founder's public keys, Syncthing-style, and each side
authorizes the other by pinning its verified keys. A wrong id only ever yields an empty prefix
or quarantined ciphertext, never someone else's plaintext.

> **Blobs are unauthenticated by design.** The `devstrap-pair1:` code carries the workspace
> id, device id, display name, OS, arch, age recipient, and signing public key — no
> fingerprint, no MAC, no signature. Integrity comes entirely from the **fingerprint
> ceremony**: the receiver derives a fingerprint from the two carried KEYS and compares it
> out-of-band before approving, so tampering with either key changes the derived fingerprint
> and fails the ceremony. The non-key fields are *not* fingerprint-bound — tampering with them
> cannot forge trust, but it can break convergence **visibly** (quarantined events, or a
> `doctor`-detected wrong hub prefix) until you re-run the ceremony with a fresh code. Any
> non-key tamper is therefore self-announcing breakage, never silent compromise.

### E.1 Founder — found the workspace and publish the pairing material

```bash
devstrap init ~/Code                 # mints the workspace id; does NOT self-mint a WCK yet
# point sync at the hub: `hub: r2://<bucket>` in ~/.devstrap/config.yaml, plus
# DEVSTRAP_HUB_S3_ENDPOINT (or `?endpoint=` on the URI) — see section A.4/B
devstrap hub login                   # store the R2/S3 secret in the keychain slot
devstrap sync                        # founds epoch 1 against the empty hub and pushes the namespace map
devstrap status                      # the `Workspace ID:` line (also `--json` → workspace_id)

devstrap devices pairing-code        # stdout: devstrap-pair1:...  stderr: founder fingerprint + next steps
```

Share the **non-secret** `devstrap-pair1:` blob (stdout) with the second device by any channel,
and read the founder **fingerprint** (stderr) aloud over a trusted channel — the joiner must
confirm it character-for-character. `devstrap devices recipient --fingerprint` prints the same
fingerprint if you need it again for a script or a check.

### E.2 Joiner — adopt the id and pin the founder in one step

Run the code-adopting init **first** — before `hub login` (E.3) — passing the founder
fingerprint you confirmed out-of-band:

```bash
devstrap init ~/Code --join --code '<founder-code>' --fingerprint <founder-fingerprint>
```

`--code` implies `--join`, adopts the founder's workspace id, and enrolls + pins the founder
row in one command. With `--fingerprint`, a mismatch fails **before any filesystem write**. Two
fallbacks keep it usable without the flag:

- **Interactive (TTY):** omit `--fingerprint` and DevStrap prints the derived fingerprint and
  asks you to type `yes` after you confirm it out-of-band.
- **Non-interactive (no TTY):** init stays scriptable — it stores the founder as **pending** and
  prints the exact `devstrap devices approve <founder-device-id> --fingerprint <derived-fp>`
  follow-up. Run that follow-up (after confirming the fingerprint) **before** the first
  `devstrap sync`.

The manual, code-free fallback (`devstrap init ~/Code --join --workspace-id <id>` then a full
`devstrap devices enroll <founder-device-id> … --approve --fingerprint <fp>`) is still supported
for recovery and tests; its flags are documented in `13_CLI_DAEMON_API.md` (`init`, `devices
enroll`). A bare `devstrap init --join` with neither `--code` nor `--workspace-id` still
initializes but warns that r2/s3 hubs key by workspace id and will not converge until you adopt
one.

<!-- MD028 separator between adjacent blockquotes -->

> **Fleets larger than two devices:** pinning any one device flips verification fail-closed,
> and device records are not synced — so pin **every** existing device this way, not just the
> founder. Events signed by a device you have not pinned yet quarantine as visible
> `event_verification_failure` conflicts and replay automatically once you enroll + approve
> that device (`conflicts list` shows them; nothing is lost).

### E.3 Joiner — log in to the hub (order matters)

```bash
# same hub config as the founder: `hub: r2://<bucket>` in ~/.devstrap/config.yaml
# plus DEVSTRAP_HUB_S3_ENDPOINT — `hub login` stores only the credential pair
devstrap hub login   # store the R2/S3 secret — AFTER the id-adopting init in E.2

devstrap devices pairing-code   # the joiner's own code + fingerprint, to send back to the founder
```

> **Keychain ordering trap.** The hub S3 credential slot is keyed on the workspace id
> (`hub-s3.<workspace_id>`). Run the id-adopting `init --join --code …` (or `--workspace-id …`)
> **first**, then `hub login`. If you `hub login` before adopting the id (or re-initialize under
> a different id afterward), the credential lands in the wrong slot and is orphaned — just
> `hub login` again under the adopted id.

Send the joiner's `devstrap-pair1:` code to the founder and read its fingerprint aloud.

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
