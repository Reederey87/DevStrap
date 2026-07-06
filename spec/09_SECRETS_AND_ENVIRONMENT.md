---
last_reviewed: 2026-07-03
tracks_code: [internal/childenv/**, internal/cli/devices.go, internal/cli/env.go, internal/cli/run.go, internal/devicekeys/**, internal/envbundle/**, internal/envfile/**, internal/platform/**, internal/workspacekeys/**]
---
# Secrets and Environment Design

## Principle

DevStrap should make env variables available everywhere without casually syncing plaintext `.env` files.

## Two supported modes

### Mode A — Personal encrypted env sync

Best for solo developer/homelab use.

Flow:

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap run work/acme/api -- npm test
```

Behavior:

- reads local `.env` once;
- parses variables with DevStrap's non-interpolating grammar;
- encrypts values for approved devices;
- syncs encrypted bundle through Hub;
- hydrates on another device only after device approval;
- can write local `.env.local` or inject at runtime.

Current implementation covers local capture, hydrate, provider binding, runtime injection, local OS-backed device private-key storage, manual remote-device enrollment, local device trust-state commands, and hub-backed env-bundle exchange: `devstrap env capture` parses without mutating process env, rejects dangerous variable names and interpolation-looking values unless `--literal` is explicit, encrypts the parsed bundle to the local device plus approved device age recipients, writes a `0600` ciphertext blob under `~/.devstrap/blobs`, records only `age_blob:<sha256>` references in SQLite, emits `env.profile.updated`, and adds the captured file to `.gitignore` when it is inside the project. `devstrap env hydrate --write <file>` decrypts encrypted blobs with the local device age identity or resolves 1Password `op://` refs through `op inject`, writes the requested env file atomically with mode `0600`, refuses overwrites unless `--force`, and gitignores the hydrated target when it is inside the project. `devstrap run` injects encrypted local profiles into subprocess env or delegates 1Password refs to `op run`. `devstrap devices enroll/list/approve/revoke/lost/rename/recipient` exposes local device registration and trust-state management and refuses revocation of the current local device; revoke/lost propagate fleet-wide via synced `device.revoked`/`device.lost` events (TRUST-01) while approval stays the local fingerprint ceremony; `devices recipient` prints the local device's public age recipient and signing key for manual out-of-band enrollment on another device. Automatic remote enrollment remains future work; out-of-band fingerprint confirmation and the one-paste pairing-code ceremony are shipped (`P4-SEC-04` parts 1+2, see `19_CLOUD_PROVISIONING_GUIDE.md` §E).

### Mode B — Secret-manager references

Best for team/company projects.

Flow:

```bash
devstrap env bind work/acme/api ./secrets.op.env --provider 1password --profile acme-dev
devstrap run work/acme/api -- uv run pytest
```

Behavior:

- DevStrap stores secret references, not values;
- provider CLI resolves values at runtime;
- logs redact values;
- no plaintext files written unless explicit.

## Env-bundle exchange over the Hub (`ENV-SYNC-01`, `HUB-*`)

Env values are age-encrypted into content-addressed `age_blob:<sha256>` blobs under `~/.devstrap/blobs`; `state.db` keeps only the reference and binding metadata. Captured and provider-bound profiles now sync through the same two-plane, zero-knowledge hub used by draft bundles:

- **Plane A — the signed, HLC-ordered event log** carries `env.profile.updated`: project path, profile name, provider/mode, encrypted `blob_ref` plus variable names for `devstrap_encrypted`, or provider refs for runtime-only profiles. The payload rides the `enc.v2` envelope, so var names and refs are not visible to the hub, and the event is in `mustVerifyEvent` because hydrated files and lifecycle-script environments are trust-affecting.
- **Plane B — the content-addressed encrypted blob store** carries only the ciphertext. `devstrap sync` uploads any referenced local env blob the hub lacks and downloads referenced env blobs the local device lacks, keyed by SHA-256. Hydrate before the blob arrives preserves the file-not-found error class and adds the remedy `run 'devstrap sync' to pull it`.

Env profile replay is LWW by the profile row's source-event coordinate: the highest `(HLC, device_id, event_id)` wins, matching namespace reconciliation. Apply distinguishes a winning tombstone from an unapplied project (dual-review fix): a tombstoned path drops the pointer (a re-add + re-capture re-emits it), while an absent path without a tombstone quarantines the event as a replayable `env_pending_project` conflict — the cursor advances, the batch never aborts, and `ReplayPendingEnvProfileConflicts` (run after every pull apply and after `devices approve` replay) recovers the profile once the project lands.

Because blobs are encrypted client-side to the enrolled device recipient set before upload (see *Encryption*), the hub stays zero-knowledge: it sees only opaque event carriers and `age_blob:<sha256>` ciphertext. Repo content never uses this path — it rides git's own blobless (`--filter=blob:none`) clone/fetch transport from each repo's existing remote — and `.git`, `node_modules`, and build artifacts are never placed in the blob store.

Device revoke/lost re-encrypts affected env and draft blobs to the reduced recipient set and flags exposed env values for rotation (see *Device trust* and *Encryption*). Env rewrap now emits superseding `env.profile.updated` events before hub cleanup, then uploads the new ciphertext and deletes or queues the superseded hub blob. `secret_bindings.needs_rotation` clearing remains device-local/operator-controlled, and the superseding upsert carries each var's flag forward (a rewrap must never silently clear a rotation warning — dogfood-found fix, `TestUpsertEnvProfileTxPreservesNeedsRotation`). Snapshots carry an env-profile pointer per entry (`SnapshotEnv`), merged on import by its own source-event coordinate, so profiles survive event-log compaction and snapshot bootstrap; recovery pulls the referenced env blobs alongside draft blobs.

Backend is Cloudflare R2 from the start, pluggable behind one `Hub` interface, with a file-backed local backend kept only for tests. The R2/S3 `aws-sdk-go-v2` adapter is wired behind `hub: r2://<bucket>` (`P5-HUB-01`); credentials come from `DEVSTRAP_HUB_S3_*` env/config, never the URI or `state.db`.

## Supported providers

MVP provider priority:

1. 1Password CLI for team/company projects and runtime-only policies;
2. Doppler CLI;
3. Infisical CLI;
4. DevStrap encrypted personal store for solo/homelab projects;
5. generic `.env.template` + shell command adapter.

## Env profile

Example:

```yaml
id: api-dev
provider: 1password
mode: runtime
bindings:
  DATABASE_URL: op://Engineering/App/database_url
  STRIPE_API_KEY: op://Engineering/Stripe/api_key
  OPENAI_API_KEY: op://Engineering/OpenAI/api_key
```

Personal encrypted mode:

```yaml
id: home-automation-dev
provider: devstrap_encrypted
mode: hydrate_or_runtime
bundle_id: envb_01jz...
bindings:
  HOME_ASSISTANT_TOKEN: encrypted
  MQTT_PASSWORD: encrypted
```

## Runtime injection

Preferred command:

```bash
devstrap run work/acme/api -- uv run pytest
```

Algorithm:

```text
1. Resolve project.
2. Resolve env profile.
3. For provider refs, write a temporary `0600` refs file and delegate to the provider CLI.
4. For encrypted local bundles, decrypt into an in-memory child env map only.
5. Build child environment with the shared sanitizer.
6. Run command as subprocess.
7. Remove temporary refs files and clear process-local secret maps.
```

Current implementation supports `devstrap run` for encrypted local profiles and 1Password reference profiles. `devstrap env bind <path> <refs-file> --provider 1password` parses a refs file, stores only `op://` references in SQLite, and gitignores the refs file when it is inside the project. Provider runs execute `op run --env-file <temp-refs-file> -- <command>` with only the basic child-process allowlist plus `OP_*` authentication variables. Provider file hydration uses `op inject --in-file <temp-refs-file> --out-file <temp-output> --file-mode 0600 --force`, then atomically installs the resolved file through the same overwrite guard as encrypted hydration. Encrypted runs decrypt the local age blob and inject plaintext only into the subprocess environment.

## Hydration to file

Sometimes tools require `.env` files.

Command:

```bash
devstrap env hydrate work/acme/api --write .env.local
```

Rules:

- default file is `.env.local`, not `.env`;
- generated file contains header warning;
- file permission `0600`;
- path must be ignored by `.gitignore` and `.devstrapignore`;
- never write secrets without explicit command.

Generated header:

```text
# Generated by DevStrap. Do not commit.
# Source profile: api-dev
# Generated at: 2026-06-23T12:00:00Z
```

Status (`SECR-01`, `SECR-02`, `SECR-05`): env hydrate now quotes safely, emits the generated-file header, writes atomically with mode `0600`, and ensures the hydrated target is ignored before secret content is written. Remaining follow-up: route ignore updates through the planned `.devstrapignore` compiler once `DRAFT-03` lands so `.gitignore`, scanner, watcher, agent deny, and bundle exclusions share one policy source.

## Env schema

**Status: PLANNED, not built.** No `env check` command exists yet (`internal/cli/env.go` has only `capture`/`hydrate`/`bind`/`rotate`); `.env.schema`/`.env.template` validation is future work.

Each project should have `.env.schema` or `.env.template`.

Example:

```dotenv
DATABASE_URL=required
STRIPE_API_KEY=required
OPENAI_API_KEY=optional
SENTRY_DSN=required
```

DevStrap validation:

```bash
devstrap env check work/acme/api
```

Output:

```text
✓ DATABASE_URL mapped
✓ STRIPE_API_KEY mapped
⚠ OPENAI_API_KEY optional missing
✗ SENTRY_DSN required but missing
```

## Device trust

Each device has an age X25519 identity for encrypted bundle recipients and an Ed25519 identity for event signing.

```text
device age public key → devices.public_key and Hub enrollment record
device age private identity → local protected secret storage
device signing public key → devices.signing_public_key and Hub enrollment record
device signing private identity → local protected secret storage
```

Env bundles are encrypted for approved device public keys.

Current implementation generates age X25519 and Ed25519 identities during `devstrap init`, stores only public keys in SQLite, and stores private identities through the platform keychain adapter. Darwin uses macOS Keychain through the Go keyring backend; Linux uses Secret Service/keyring through the same backend. If the system keyring is unavailable, DevStrap falls back to `~/.devstrap/keys` with mode `0600` so headless/CI systems remain usable.

### Key-custody model (`P6-XP-04`)

Where a device keeps its secret material is a **recorded, honored decision**, not a per-error guess. Three properties hold:

- **Typed classification, not string matching.** The platform seam (`internal/platform`, `mapKeyringError`) is the single place that turns the keyring library's error vocabulary into typed sentinels: `ErrSecretNotFound` (the backend is reachable but holds no such secret — a mint may proceed) versus `ErrUnsupported` (the backend is unreachable/unsupported — including a missing Linux Secret Service / D-Bus session, which go-keyring surfaces as an untyped godbus error that the seam classifies here). `internal/devicekeys` consumes those sentinels with `errors.Is`; it never inspects error strings. A live-backend hard failure stays untyped and fails closed.
- **Never mint over a published identity.** `EnsureSigning`/`Ensure` receive the device's already-published public key and refuse to mint a replacement when the keychain is unreachable (the split-custody wedge) or when a key is published but its private half is absent from a reachable backend — either would diverge from `devices.signing_public_key`. The refusal carries the remedy (run from a desktop session, or set `DEVSTRAP_NO_KEYCHAIN=1` and migrate the key file). The same never-mint-over-held principle guards the WCK custody path.
- **Recorded custody decision.** At init the decision is recorded once in the local, never-synced `local_meta` table (migration `00016`) and never rewritten, but only from *safe evidence* so an existing store is never stranded: `file` when `DEVSTRAP_NO_KEYCHAIN=1` (explicit operator choice); `keychain` when the probe positively finds the keychain reachable (safe regardless of pre-existing secrets); and `file` from an unreachable probe *only* for a genuine first init (a brand-new device with no already-published keys). Crucially, an unreachable probe on an *already-initialized* store — e.g. a pre-`00016` store whose secrets live only in the keychain, first run headless after upgrade — records nothing and stays `CustodyUnset`, so a later desktop run still reads the keychain where the real secrets are. Later runs honor a recorded decision: a `file`-custody store keeps using files even if a keychain appears; a `keychain`-custody store refuses to silently degrade to file custody (it fails closed with a remedy) unless `DEVSTRAP_NO_KEYCHAIN=1` forces file custody. `doctor` reports the recorded backend and warns when it is currently unreachable, overridden, or unrecorded. Under legacy (unrecorded) custody the historical hybrid behavior is preserved — prefer the keychain, fall back to the file store when unavailable and nothing is published — so the mint guard above still prevents a divergent mint. All device/workspace/hub-credential key stores are stamped with the recorded custody, so a stale keychain entry can never shadow the authoritative file key on a file-custody machine. The same `HybridStore` also custodies Workspace Content Keys (WCK) for event-log envelope encryption (`P4-SEC-07`/`P6-SEC-02`, keyed `wck.<workspace_id>.<epoch>.<kid>` where `kid = hex(sha256(wck))`; the pre-kid `wck.<workspace_id>.<epoch>` form is the legacy slot, lazily upgraded by `Keyring.Prime`). The kid is validated (64 lowercase hex chars or empty) before it reaches any keychain account name or file path. Manual `devices enroll --approve` records an approved device age recipient so future captures include that recipient, and grants every held WCK epoch's fleet key to the newly-approved device. Synced env blobs work with manual enrollment; automatic remote enrollment remains future work. Fingerprint confirmation is shipped (`P4-SEC-04`) and gates every approval that adds a recipient.

Device states:

```text
pending
approved
revoked
lost
```

New device approval:

```bash
devstrap devices approve dev_linux_desktop
```

Approval requires out-of-band fingerprint verification. The approving device shows the public key fingerprint advertised by the Hub, and the user must confirm that it matches the new device before the new key can receive bundles. A mismatch means the Hub may be substituting keys and approval must fail.

Periodic WCK rotation (`keys rotate` / age-triggered in `sync`, default 90d — P4-SEC-07) is distinct from SECRET rotation: it re-keys the namespace-map event log going forward and never touches env bundles, blobs, or `needs_rotation` flags. Device add, revoke, lost, or rotate events trigger re-encryption of affected bundles to the current approved-recipient set. Re-encryption removes future access to stored bundle ciphertext but does not make previously exposed secret values safe; revocation workflows must also mark affected values as requiring provider-side or service-side value rotation. At least one approved device must retain recoverable plaintext for every bundle before revocation completes.

`devstrap env rotate <path> <env-file>` re-captures and re-encrypts a profile to the current approved recipient set and clears its `needs_rotation` flags; `devstrap env rotate <path>` clears the flags for that project without re-capturing, and `devstrap env rotate --all` clears flags workspace-wide. The per-project flag-clear path now joins through `namespace_entries.env_profile_id` (`P6-DATA-02`, shipped 2026-07-03) so it does not depend on a phantom `env_profiles.namespace_id` column.

## Secret redaction

Secrets are represented in code as a capability, not as ordinary log-bound strings. A secret type must render as `***` for `String`, `GoString`, and JSON marshaling; plaintext is available only through an explicit audited reveal path at the subprocess boundary.

Redaction is a backstop for:

- exact secret values;
- common token formats;
- env vars marked secret;
- provider references if configured;
- `.env` file contents.

Log output should show:

```text
DATABASE_URL=***
OPENAI_API_KEY=***
```

If a subprocess receives secrets, raw stdout/stderr persistence is disabled unless the log stream is scrubbed and marked as tainted. Tests must assert that a loaded secret value cannot be found in logs, event payloads, or `state.db`.

## Agent secret policy

Agents should get minimal env.

Example:

```yaml
agent_env:
  allow:
    - GITHUB_TOKEN_READONLY
    - DATABASE_URL
  deny:
    - AWS_SECRET_ACCESS_KEY
    - SSH_PRIVATE_KEY
    - OPENAI_ADMIN_KEY
```

Agent default:

```text
No secrets unless explicitly allowed by project agent policy.
```

Child process environments start empty; DevStrap must not inherit `os.Environ()` by default. Allowed names are resolved from the bound env profile, denied names are removed, and dangerous names are stripped last and unconditionally:

```text
LD_PRELOAD
DYLD_INSERT_LIBRARIES
BASH_ENV
NODE_OPTIONS
PYTHONPATH
GIT_SSH_COMMAND
```

Current implementation provides `internal/childenv`, a shared allowlist-based child environment builder. Git subprocesses and editor launches use it to avoid wholesale inheritance, and dangerous names are non-overridable even when allowlisted. Env-profile resolution, provider injection, and agent policy binding remain future work.

## Secret scanning

During scan, detect dangerous files:

```text
.env
.env.*
*.pem
id_rsa
id_ed25519
credentials.json
service-account*.json
.snowflake/config.toml
```

Behavior:

- warn;
- offer env capture;
- add ignore rules;
- never upload as draft content.

## Encryption

Decision: use age v1 (`filippo.io/age`) for encrypted env and draft bundles.

Rules:

- one recipient stanza per approved device X25519 public key;
- payload encryption uses age defaults, currently ChaCha20-Poly1305;
- bundle metadata binds `bundle_id` and `workspace_id` in a signed manifest header;
- device private identities are stored in OS keychain/Secret Service when available with a `0600` file fallback for unsupported/headless systems;
- adding, removing, revoking, or rotating devices re-encrypts affected bundles to the new recipient set and creates explicit value-rotation follow-up work for any secret that may have been exposed to a revoked/lost device;
- `encrypted_value_ref` stores a content-addressed pointer such as `age_blob:<sha256>`, never a plaintext value;
- passphrase-only encryption is not acceptable for Hub-synced personal bundles because the Hub must not be able to decrypt and recipients are per device.

## Policy examples

Personal project:

```yaml
secrets:
  mode: encrypted_sync
  write_file_default: .env.local
  approved_devices:
    - macbook
    - linux-desktop
```

Company project:

```yaml
secrets:
  mode: runtime_only
  provider: 1password
  write_file_default: never
  require_schema: true
```

Agent project:

```yaml
secrets:
  mode: runtime_only
  agent_default: none
  agent_allow:
    - GITHUB_TOKEN_REPO_SCOPED
```

## Audit implementation notes (2026-06-28)

- **SECR-01**: `quoteDotenv` now emits POSIX single-quoted values (literal in every dotenv loader) for values without newlines; multiline values escape `$` and backtick. `looksInterpolated` flags bare `$VAR` so `$`-containing values require `--literal`.
- **SECR-02**: Hydrated env files now begin with `# Generated by DevStrap. Do not commit.` header with profile name and timestamp.
- **SECR-04**: Key custody fallback (`HybridStore`) gates file storage on keychain reachability; a present-but-failing keychain fails closed. `slog.Warn` fires when the file fallback is taken. **Refined by `P6-XP-04` (2026-07-03):** the reachability test is now the typed `errors.Is(err, platform.ErrUnsupported)` / `ErrSecretNotFound` split (see the key-custody model above), replacing the previous `err.Error()` substring heuristic that misclassified a dead D-Bus session as "not found" and could mint a divergent signing key on headless Linux.
- **SECR-05**: `env hydrate` calls `ensureIgnored` before writing the secret content.
- **CODE-04**: `writeEnvBlob` uses named return + deferred Close observation + `file.Sync()` for durability.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-GIT-03 — Dependency rebuild runs untrusted postinstall scripts after `.env` hydration

**Problem.** `materializeGitRepo` calls `hydrateProjectEnv` (writing cleartext `.env` into `localPath`) *before* `runRebuildCommand`, which runs `npm ci`/`pnpm install`/etc. with `HOME: dir` and discarded output (`internal/cli/materialize.go:198,205-208,361-362,371`), so a malicious postinstall can `cat $HOME/.env` at the freshly decrypted secrets with no forensic trail. The env-var gate is not the per-project `rebuild_on_hydrate: ask|always|never` and no 0600 log exists (spec/08:105,108).

**Actionable steps.**
1. Swap the calls so `rebuildDependencies` runs before `hydrateProjectEnv` in `materializeGitRepo`.
2. Capture rebuild stdout/stderr to a 0600 log under `~/.devstrap/logs/rebuilds/<project>.log`.
3. Implement the per-project `materialization.rebuild_on_hydrate` policy or reconcile spec/08:105 with the env-var gate.
4. Test that `.env` does not exist at rebuild time.

**Example.**
```go
// materializeGitRepo: rebuild BEFORE hydrating secrets into the tree
if err := rebuildDependencies(ctx, dir); err != nil { /* logged to 0600 log */ }
if err := hydrateProjectEnv(ctx, project, dir); err != nil { return err }
```

### P6-DATA-02 — `ClearRotationForProject` filters on a non-existent `env_profiles.namespace_id` — **shipped (2026-07-03)**

**Was.** The one-arg `devstrap env rotate <path>` (flag-clear-only) ran a subquery `SELECT id FROM env_profiles WHERE namespace_id = ?`, but `env_profiles` has no `namespace_id` column; the link is `namespace_entries.env_profile_id`. Every invocation failed with `no such column: namespace_id` → "clear rotation for project: SQL logic error"; only `env rotate --all` was tested.

**Shipped fix.** `ClearRotationForProject` now filters `secret_bindings` by `namespace_entries.env_profile_id` for the requested namespace entry. `TestClearRotationForProject` covers two projects with encrypted profiles and proves clearing one project leaves the other flagged; `TestEnvRotateProjectClearsRotationFlag` covers the one-arg CLI form and its success message.

**Remaining follow-up.** Add a CI lint that `db.Prepare`s every static query in `store.go` against a migrated in-memory DB.

**Example.**
```sql
UPDATE secret_bindings SET needs_rotation = 0, updated_at = ?
WHERE needs_rotation = 1
  AND env_profile_id IN (
    SELECT env_profile_id FROM namespace_entries
    WHERE id = ? AND env_profile_id IS NOT NULL);
```

### P6-DATA-04 — `db backup` produces an incomplete, unrestorable workspace backup — **SHIPPED 2026-07-04 (`fix/p6-data-04`)**

**Resolved.** `db backup --full <out.tar>` captures `state.db` (`VACUUM INTO`) + the `blobs/<sha256>.age` files `AllBlobRefs` reports + key material + `config.yaml`, all `0600`. Key capture is custody-aware: file custody copies `KeyDir` (asserting the device age + signing basenames are present, hard error otherwise); keychain custody escrows via `devicekeys.HybridStore.ExportForBackup` — the device age + Ed25519 signing identities + **every held WCK epoch** (enumerated from `HeldKeys`) + hub S3 creds, with a hard error naming any unreadable required key so a "full" backup can never be silently incomplete. `db restore <in.tar>` refuses a non-empty state dir without `--force`, validates the staged DB before promoting, and swaps only the captured targets in place (preserving un-captured Home data), with a zip-slip guard. A doctor dangling-blob-refs check and a keychain-custody restore warning ship too. The keychain-custody restore caveat (restored DB still records `keychain`; operator runs under `DEVSTRAP_NO_KEYCHAIN=1` or re-migrates) is surfaced at restore and documented in spec/12.

**Original problem (now fixed).** `Backup` was `VACUUM INTO` + chmod + `validateBackup` — the SQLite file only. Encrypted env values live outside the DB as `~/.devstrap/blobs/<hash>.age` and key fallback lives in `<statedir>/keys`; there was no restore command, yet `doctor` recommended "restore from a `devstrap db backup`." Restoring only `state.db` left dangling `age_blob:` refs and unrecoverable secrets.

**Actionable steps (done).**
1. Ship `devstrap db backup --full <out.tar>` (state.db + referenced `blobs/` + `keys/` when file-fallback active + keychain export/escrow in default mode, all 0600) and `devstrap db restore <in>` (refuse over a non-empty state dir without `--force`).
2. Add a doctor "dangling blob refs" check over `AllBlobRefs` (stat local blob, fall back to hub `HasBlob` for draft refs).
3. Fix the `doctor.go:203-205` remedy text once `--full` exists.

**Example.**
```bash
devstrap db backup --full ~/devstrap-recovery.tar   # state.db + blobs/ + keys/ + keychain escrow (0600)
devstrap db restore ~/devstrap-recovery.tar         # refuses non-empty state dir without --force
```
