---
last_reviewed: 2026-06-28
tracks_code: []
---
# Cloud Provisioning & Configuration Guide

> **Status: FUTURE direction (`HUB-*`, `SCALE-*`).** Nothing in this guide is built or
> wired this cycle. It is the provisioning runbook for when the cloud/SaaS build lands —
> and it doubles as a single-owner deployment recipe usable today. The only shipped sync
> transport remains `devstrap sync --hub-file <path>` (the file-backed test hub). Every
> CLI flag, env var, and config key marked *planned* below names the intended surface for
> the cloud hub; treat names as provisional until `HUB-*` ships. See
> `AUDIT_RECOMMENDATIONS_2026-06-28.md` (decisions 5 and 6) and `14_MVP_ROADMAP_AND_BACKLOG.md`.

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
2. **R2 is zero-knowledge.** Everything stored there is client-side age-encrypted and
   signed before upload, so R2 holds ciphertext plus a signed map and can read neither
   code, secrets, nor drafts. Tenant isolation for the eventual SaaS therefore falls out
   of the encryption model, not out of access-control lists (`15_SECURITY_THREAT_MODEL.md`).

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

One bucket holds every workspace. Tenants are separated by **key prefix**, never by
separate buckets:

```text
s3://devstrap-hub/<workspace_id>/events/<hlc>-<seq>.json   # Plane A — signed event log
s3://devstrap-hub/<workspace_id>/blobs/<sha256>            # Plane B — age ciphertext
```

`<workspace_id>` is the local `ws_<uuidv7>` identity minted during `devstrap init`. Because
every object under a prefix is already encrypted and the map signed, prefix-level
separation is sufficient for isolation; access scoping (below) is defense in depth.

### A.3 Create an S3-compatible API token (least privilege)

1. **R2 → Manage R2 API Tokens → Create API Token.**
2. Permission: **Object Read & Write** (not Admin). Do **not** grant account-wide or
   bucket-management rights.
3. Scope: **Apply to specific buckets only → `devstrap-hub`**. This is the least-privilege
   boundary — the token can read and write objects in exactly one bucket and nothing else.
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
- **The Secret Access Key is a secret** and goes through DevStrap's existing encrypted
  secrets path only: OS keychain / Secret Service, an age-encrypted blob, a 1Password
  `op://` ref, or — for the server side — a Fly secret. Never plaintext config, never a
  committed file, never a log line.

Planned client invocation (a developer box running sync):

```bash
# planned (HUB-*): the local test hub flag --hub-file stays for tests only
devstrap sync --hub-s3 devstrap-hub
```

Planned config / env names (provisional until `HUB-*` lands). Non-secret settings:

```text
DEVSTRAP_HUB_S3_BUCKET=devstrap-hub                                  # planned
DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com  # planned
DEVSTRAP_HUB_S3_REGION=auto                                          # planned
```

Secret values — supply via the secrets path, **not** as plaintext env in a shell profile:

```text
DEVSTRAP_HUB_S3_ACCESS_KEY_ID                                        # planned (id; low sensitivity, still not committed)
DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY                                    # planned (secret; keychain / age blob / op:// / Fly secret)
```

Because R2's API is S3-compatible, the underlying client also honors the standard AWS SDK
names (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION=auto`,
`AWS_ENDPOINT_URL_S3=https://<ACCOUNT_ID>.r2.cloudflarestorage.com`). On the server side
these are injected from Fly secrets at runtime (section B); on a developer box, store the
secret access key as a 1Password `op://` ref or an age-encrypted blob and let DevStrap
resolve it at sync time — the same machinery already used for `devstrap env`.

DevStrap owns object lifecycle: blob **ref-counting** and garbage collection of unreferenced
`age_blob:<sha256>` objects happen client-side after device revoke/lost re-encryption
(`09_SECRETS_AND_ENVIRONMENT.md`), so no R2-side object-lifecycle rule is required.

### A.5 Cost note

R2's free tier (10 GB-month storage, plus monthly Class A/B operation allowances) covers a
solo fleet comfortably, and **egress is always free** — the property that makes
clone-from-anywhere cheap. A card is required to enable R2 even while you stay free.

---

## B. Fly.io — compute (control plane + agent runners)

Fly.io runs **both** planes: a small Go control-plane API service as a long-lived app, and
**ephemeral per-task agent-runner Machines** (Firecracker microVMs). It is chosen for
microVM isolation (safe for untrusted agent code), 35+ regions, scale-to-zero /
suspend-resume, and native execution of the Go binary. E2B (self-hostable microVM agent
sandboxes) is the documented runner escape-hatch (`03_SYSTEM_ARCHITECTURE.md`).

### B.1 Install flyctl and authenticate

```bash
brew install flyctl              # or: curl -L https://fly.io/install.sh | sh
fly version
fly auth signup                  # first time; or `fly auth login`
```

Add a payment method in the Fly dashboard billing page. Fly is pay-as-you-go; a small
control-plane Machine costs cents/day and scale-to-zero keeps an idle deployment near zero.

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

Runners are **not** part of the long-lived app's autoscaling. Each task gets its own
Firecracker microVM created on demand and destroyed when the task ends — strong isolation
for untrusted multi-tenant code, **one Machine per task / tenant**:

```bash
# create a throwaway runner microVM for a single task
fly machine run <runner-image> \
  --app devstrap-hub-runners \
  --region iad \
  --rm \
  --env DEVSTRAP_AGENT_RUN_ID=<run-id>
```

For bursty agents, suspend/resume keeps warm capacity without paying for idle:

```bash
fly machine suspend <machine-id>
fly machine resume  <machine-id>
```

In production these are driven programmatically via the **Fly Machines REST API**
(`https://api.machines.dev/v1/apps/<app>/machines`) using a Fly access token held as a
control-plane secret, so the control plane spins runners up and tears them down per task.
Runners inherit secrets the same way — injected at boot, never in the image — and should
receive only the minimal scoped credentials a single task needs.

### B.4 Regions, scaling, one platform

Pick `primary_region` near the operator and the bulk of devices; add regions later by
scaling the app or launching runner Machines in other regions (35+ available). The same
Fly account/org hosts both planes — control-plane app and runner Machines — which keeps
networking, secrets, and observability in one place. Multi-tenant scaling principles
(control/data-plane split, pooled→dedicated tenancy spectrum, cell-based scaling) are
recorded as future direction in `03_SYSTEM_ARCHITECTURE.md` and
`14_MVP_ROADMAP_AND_BACKLOG.md` (`SCALE-*`).

### B.5 Cost note

Pay-as-you-go. A single `shared-cpu-1x` / 256 MB control-plane Machine with scale-to-zero
costs roughly cents per day idle; runner microVMs bill only for their short lifetime.
There is no perpetual free tier — budget a few dollars/month for a solo deployment.

---

## C. Neon — managed Postgres (control-plane database)

Neon is the **control-plane database** for the future multi-user/SaaS direction: accounts,
devices, billing, metering, and the tenant directory. It is **not** the sync data plane —
no code, secrets, drafts, or the namespace map live here; those are R2 + git. Neon is
chosen for serverless Postgres, scale-to-zero, and branchable databases for preview/test.

### C.1 Sign up and create the project

1. Sign up at `https://neon.com` (Neon console).
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

### C.3 Create a least-privilege application role

Do **not** ship the project owner role to the service. Create a dedicated application role
with only the privileges the control plane needs:

```sql
CREATE ROLE devstrap_app LOGIN PASSWORD '<generated>';
GRANT CONNECT ON DATABASE <db> TO devstrap_app;
GRANT USAGE ON SCHEMA app TO devstrap_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA app TO devstrap_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO devstrap_app;
-- no SUPERUSER, no CREATEROLE, no DDL on production schemas
```

Build the service's `DATABASE_URL` from this role, not the owner.

### C.4 Configure DevStrap with Neon

Store the application-role connection string as a **Fly secret** named `DATABASE_URL`
(section B.2) so it is injected at runtime and never committed or logged. It is a
control-plane secret and follows the same custody rules as every other credential here.

### C.5 Branching and multi-tenancy

- **Branching** — Neon branches are copy-on-write database clones; use one per preview/CI
  environment so test runs never touch production data. A test Fly app simply points its
  `DATABASE_URL` at the branch's connection string.
- **Tenancy model** — for the eventual SaaS, choose between **schema-per-tenant**
  (stronger isolation, heavier migration fan-out) and **shared tables with a `tenant_id`
  column** (simpler operations, isolation enforced in app + row-level security). This is a
  control-plane choice only; the zero-knowledge data plane already isolates tenants by
  construction regardless (`15_SECURITY_THREAT_MODEL.md`, `SCALE-*`).

### C.6 Cost note

Neon's free tier (one project, generous storage and compute-hour allowance, scale-to-zero)
covers a solo control plane; paid tiers add more branches, larger compute, and longer
history when the SaaS direction is built.

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
  - *Client side* (a developer box running `devstrap sync`): all secrets go through
    DevStrap's **existing encrypted secrets path** — OS keychain / Secret Service, an
    age-encrypted `age_blob:<sha256>` blob under `~/.devstrap/blobs`, or a 1Password
    `op://` ref resolved at use time. Non-secret connection settings (bucket, endpoint,
    region, workspace prefix) may live in plain config.
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
| Cloudflare R2   | 10 GB-month storage + monthly op allowances; **zero egress** | Card required to enable; solo fleet stays free |
| Fly.io          | None (pay-as-you-go)                                   | Small control Machine ~cents/day idle; runners bill per task |
| Neon (Postgres) | One project, scale-to-zero compute + storage allowance | Paid tiers add branches/compute/history        |

### D.5 Cross-references

- `03_SYSTEM_ARCHITECTURE.md` — Hub backend interface; the *Hosting & scaling (FUTURE)*
  subsection that selects Fly.io + R2 + managed Postgres (`SCALE-*`); the planned HTTP/SSE
  wire protocol.
- `07_NAMESPACE_AND_SYNC_MODEL.md` — the namespace map / event log, HLC ordering, and the
  R2 (Option C, object-store) backend behind the pluggable `Hub` interface (`HUB-*`).
- `09_SECRETS_AND_ENVIRONMENT.md` — the encrypted secrets path, `age_blob:<sha256>` blobs,
  `op://` refs, device recipients, and blob ref-counting / GC.
- `12_DATA_MODEL_SQLITE.md` — R2 per-workspace key prefixing and the rule that hub
  connection settings are config, not schema, with no plaintext credentials in `state.db`.
- `13_CLI_DAEMON_API.md` — the planned `devstrap sync --hub-s3 <bucket>` flag and the
  `--hub-file` test-only backend.
- `15_SECURITY_THREAT_MODEL.md` — the two-plane zero-knowledge hub trust model and
  multi-tenant isolation by construction (`HUB-*`, `SCALE-*`).
- `AUDIT_RECOMMENDATIONS_2026-06-28.md` — the `HUB-*` (cloud zero-knowledge hub on R2) and
  `SCALE-*` (multi-user hosting on Fly.io + R2 + managed Postgres) workstreams that drive
  this guide.
