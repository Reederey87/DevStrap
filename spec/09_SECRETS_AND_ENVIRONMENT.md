---
last_reviewed: 2026-06-30
tracks_code: [internal/childenv/**, internal/cli/env.go, internal/devicekeys/**, internal/envbundle/**, internal/envfile/**, internal/platform/**]
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

Current implementation covers local capture, hydrate, provider binding, runtime injection, local OS-backed device private-key storage, manual remote-device enrollment, and local device trust-state commands: `devstrap env capture` parses without mutating process env, rejects dangerous variable names and interpolation-looking values unless `--literal` is explicit, encrypts the parsed bundle to the local device plus approved device age recipients, writes a `0600` ciphertext blob under `~/.devstrap/blobs`, records only `age_blob:<sha256>` references in SQLite, and adds the captured file to `.gitignore` when it is inside the project. `devstrap env hydrate --write <file>` decrypts local encrypted blobs with the local device age identity or resolves 1Password `op://` refs through `op inject`, writes the requested env file atomically with mode `0600`, refuses overwrites unless `--force`, and gitignores the hydrated target when it is inside the project. `devstrap run` injects encrypted local profiles into subprocess env or delegates 1Password refs to `op run`. `devstrap devices enroll/list/approve/revoke/lost/rename` exposes local device registration and trust-state management and refuses revocation of the current local device. The encrypted blobs are still local-only — they do not yet travel between devices. Hub-backed env-bundle exchange (`DRAFT-*`, see *Env-bundle exchange over the Hub*), out-of-band fingerprint confirmation, and automatic remote enrollment remain future work.

### Mode B — Secret-manager references

Best for team/company projects.

Flow:

```bash
devstrap env bind work/acme/api --provider 1password --profile acme-dev
devstrap run work/acme/api -- uv run pytest
```

Behavior:

- DevStrap stores secret references, not values;
- provider CLI resolves values at runtime;
- logs redact values;
- no plaintext files written unless explicit.

## Env-bundle exchange over the Hub (`DRAFT-*`, `HUB-*`)

Today env values are age-encrypted into content-addressed `age_blob:<sha256>` blobs and stored only under the local `~/.devstrap/blobs` directory; `state.db` keeps just the `age_blob:<sha256>` reference. The blobs are correctly encrypted for the approved device recipient set, but they do not yet travel between devices — so a profile captured on the Mac Mini upstairs cannot be hydrated on the GMKtec Ubuntu box. This is the open local-only gap (`DRAFT-*`).

Closing it (planned, not built) reuses the two-plane, zero-knowledge hub from `03_SYSTEM_ARCHITECTURE.md` and `07_NAMESPACE_AND_SYNC_MODEL.md` rather than introducing a new channel:

- **Plane A — the signed, HLC-ordered namespace-map event log** carries the *metadata*: which project owns which env profile, its `bundle_id`, and the `age_blob:<sha256>` reference. It never carries plaintext or ciphertext bytes, only the reference plus signed ordering.
- **Plane B — the content-addressed encrypted blob store** carries the *ciphertext*: `devstrap sync` uploads any local `age_blob:<sha256>` blob the hub is missing and downloads any referenced blob the local device lacks, keyed purely by SHA-256. The hub stores opaque ciphertext; it can decrypt nothing.

Because blobs are encrypted client-side to the enrolled device recipient set before upload (see *Encryption*), the hub stays zero-knowledge: it sees only `age_blob:<sha256>` ciphertext plus the signed map. Repo content never uses this path — it rides git's own blobless (`--filter=blob:none`) clone/fetch transport from each repo's existing remote — and `.git`, `node_modules`, and build artifacts are never placed in the blob store.

Device revoke/lost still re-encrypts affected bundles to the reduced recipient set and flags exposed values for rotation (see *Device trust* and *Encryption*); after re-encryption the new `age_blob:<sha256>` is uploaded and the superseded blob becomes unreferenced and is garbage-collected. The production exchange additionally requires automatic remote enrollment and out-of-band fingerprint confirmation, which remain future work.

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
id: snowflake-dev
provider: 1password
mode: runtime
bindings:
  SNOWFLAKE_ACCOUNT: op://Engineering/Snowflake/account
  SNOWFLAKE_USER: op://Engineering/Snowflake/user
  SNOWFLAKE_ROLE: op://Engineering/Snowflake/role
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
# Source profile: snowflake-dev
# Generated at: 2026-06-23T12:00:00Z
```

Status (`SECR-01`, `SECR-02`, `SECR-05`): env hydrate now quotes safely, emits the generated-file header, writes atomically with mode `0600`, and ensures the hydrated target is ignored before secret content is written. Remaining follow-up: route ignore updates through the planned `.devstrapignore` compiler once `DRAFT-03` lands so `.gitignore`, scanner, watcher, agent deny, and bundle exclusions share one policy source.

## Env schema

Each project should have `.env.schema` or `.env.template`.

Example:

```dotenv
SNOWFLAKE_ACCOUNT=required
SNOWFLAKE_USER=required
SNOWFLAKE_ROLE=optional
OPENAI_API_KEY=required
```

DevStrap validation:

```bash
devstrap env check work/acme/api
```

Output:

```text
✓ SNOWFLAKE_ACCOUNT mapped
✓ SNOWFLAKE_USER mapped
⚠ SNOWFLAKE_ROLE optional missing
✗ OPENAI_API_KEY required but missing
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

Current implementation generates age X25519 and Ed25519 identities during `devstrap init`, stores only public keys in SQLite, and stores private identities through the platform keychain adapter. Darwin uses macOS Keychain through the Go keyring backend; Linux uses Secret Service/keyring through the same backend. If the system keyring is unavailable, DevStrap falls back to `~/.devstrap/keys` with mode `0600` so headless/CI systems remain usable. The same `HybridStore` also custodies per-epoch Workspace Content Keys (WCK) for event-log envelope encryption (`P4-SEC-07`, keyed `wck.<workspace_id>.<epoch>`). Manual `devices enroll --approve` records an approved device age recipient so future captures include that recipient, and grants every held WCK epoch to the newly-approved device. Production synced env blobs still require automatic remote enrollment and fingerprint confirmation.

Device states:

```text
pending
approved
revoked
lost
```

New device approval:

```bash
devstrap devices approve dev_gmk_ubuntu
```

Approval requires out-of-band fingerprint verification. The approving device shows the public key fingerprint advertised by the Hub, and the user must confirm that it matches the new device before the new key can receive bundles. A mismatch means the Hub may be substituting keys and approval must fail.

Device add, revoke, lost, or rotate events trigger re-encryption of affected bundles to the current approved-recipient set. Re-encryption removes future access to stored bundle ciphertext but does not make previously exposed secret values safe; revocation workflows must also mark affected values as requiring provider-side or service-side value rotation. At least one approved device must retain recoverable plaintext for every bundle before revocation completes.

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
SNOWFLAKE_ACCOUNT=***
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
    - SNOWFLAKE_ACCOUNT
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
    - mac-mini-upstairs
    - gmk-ubuntu
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
- **SECR-04**: Key custody fallback (`HybridStore`) now gates file storage on `IsKeychainUnavailable(err)`; a present-but-failing keychain fails closed. `slog.Warn` fires when the file fallback is taken.
- **SECR-05**: `env hydrate` calls `ensureIgnored` before writing the secret content.
- **CODE-04**: `writeEnvBlob` uses named return + deferred Close observation + `file.Sync()` for durability.
