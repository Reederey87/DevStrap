---
last_reviewed: 2026-07-01
tracks_code: []
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
s3://devstrap-hub/workspaces/<workspace_id>/events/<hlc-padded>/<device_id>/<seq>/<event_id>.json
s3://devstrap-hub/workspaces/<workspace_id>/blobs/<sha256>
s3://devstrap-hub/workspaces/<workspace_id>/snapshots/<hlc-padded>.json.age
```

`<workspace_id>` is the local `ws_<uuidv7>` identity minted during `devstrap init`. Because
every object under a prefix is already encrypted and the map signed, prefix-level
separation is sufficient for confidentiality. Access scoping (below) is still required
for integrity and availability: a bucket-wide key can delete or withhold ciphertext even
though it cannot decrypt it.

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
- **The Secret Access Key is a secret.** *Target state:* it goes through DevStrap's existing
  encrypted secrets path — OS keychain / Secret Service, an age-encrypted blob, a 1Password
  `op://` ref, or — for the server side — a Fly secret. **Current state (`P6-HUB-02`): the
  client hub path resolves the secret only from a plaintext env var or a plaintext config
  line — the keychain / age-blob / `op://` resolution promised below is *not yet built*.**
  Passing `DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=op://vault/item/key` today signs the literal
  string as the AWS secret and fails with an opaque `SignatureDoesNotMatch`. Until
  `P6-HUB-02` lands, the only working custody on a developer box is a plaintext env var (see
  spec/13 and spec/15, which sanction plaintext-env custody); server-side Fly secrets inject
  a plaintext env var at runtime and work as documented.

Planned client invocation (a developer box running sync):

```bash
# shipped (P5-HUB-01): the bucket is the r2:// URI host; --hub-file stays for tests only
devstrap sync   # hub: r2://devstrap-hub
```

Config / env names (shipped, `P5-HUB-01`). The bucket is the `r2://` (or `s3://`) URI host, not a separate env var. Non-secret settings:

```text
DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com  # shipped (or ?endpoint= on the URI)
DEVSTRAP_HUB_S3_REGION=auto                                          # shipped (default: auto)
```

Secret values — supply via the secrets path, **not** as plaintext env in a shell profile:

```text
DEVSTRAP_HUB_S3_ACCESS_KEY_ID                                        # shipped (id; low sensitivity, still not committed; AWS_ACCESS_KEY_ID fallback)
DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY                                    # shipped as PLAINTEXT env/config only; keychain / age blob / op:// resolution is PLANNED (P6-HUB-02). AWS_SECRET_ACCESS_KEY fallback
```

Because R2's API is S3-compatible, the underlying client also honors the standard AWS SDK
names (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION=auto`,
`AWS_ENDPOINT_URL_S3=https://<ACCOUNT_ID>.r2.cloudflarestorage.com`). On the server side
these are injected from Fly secrets at runtime (section B). On a developer box the *target*
is to store the secret access key as a 1Password `op://` ref or an age-encrypted blob and
let DevStrap resolve it at sync time — the same machinery already used for `devstrap env` —
but that resolution is **not yet wired into the hub path** (`P6-HUB-02`); today the client
reads the secret literally from the env var / config line.

DevStrap owns object lifecycle: blob **ref-counting** and garbage collection of unreferenced
`age_blob:<sha256>` objects happen client-side after device revoke/lost re-encryption
(`09_SECRETS_AND_ENVIRONMENT.md`), so no R2-side object-lifecycle rule is required.

> **Runbook caveat (`P6-HUB-01`): do not run `devstrap hub gc` against a live bucket until GC
> is sync-first and grace-windowed.** Today GC computes reachability from local state only;
> against a shared/real bucket that can delete blobs another device still references before
> this device has pulled the map. Pull the latest event log first and skip objects newer than
> a grace window before deleting. Prefer `--dry-run` until `P6-HUB-01` ships.

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
- [ ] R2 **Object Read & Write** token scoped to `devstrap-hub` only; Account ID + keys + endpoint captured into the secrets path (not a file).
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
| Cloudflare R2   | Hub **data plane** (zero-knowledge)          | Signed event log + `age_blob:<sha256>` ciphertext | Secret key via keychain / age blob / `op://` / Fly secret |
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

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-HUB-02 — Hub S3 credential custody contradicts this guide (only plaintext env/config works)

**Problem.** `selectBackendHub` (`internal/cli/hub.go:106-110`) reads the secret literally
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

