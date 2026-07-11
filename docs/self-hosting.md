# Self-hosting a DevStrap hub

The DevStrap hub is where your devices exchange the signed namespace map and the encrypted
env/draft blobs. It is **zero-knowledge**: repo content rides Git's own transport and never
reaches the hub, and everything the hub does hold is either an Ed25519-signed, envelope-
encrypted event or an age-encrypted content-addressed blob. The carrier sees only ciphertext
plus routing metadata — never your code, secrets, or drafts. That is what makes even a "dumb"
storage backend a safe boundary.

You choose a carrier by setting `hub:` in `~/.devstrap/config.yaml` (or the `DEVSTRAP_HUB` env
var). There are three, all behind one pluggable interface, and every device in a workspace must
point at the same one.

## Choosing a carrier

| Carrier | Config value | Reach for it when |
|---|---|---|
| **Git repo** (default) | `hub: git+ssh://git@host/you/devstrap-hub.git` | You want zero infrastructure — any private repo you can already push to. |
| **Folder** | `hub: folder:/abs/path/to/shared/dir` | You already have a synced cloud-drive folder or a network mount. |
| **R2 / S3** | `hub: r2://<bucket>` | You need scale: blobs over a forge's per-object limit, high push rates, or object-storage economics. |

A file-backed carrier (`hub: file:<path>` / `--hub-file <path>`) also exists, but it is for
**local tests only** — never production.

### Git carrier (recommended default)

Any private Git repo becomes the hub. Create an empty private repo and register it:

```bash
gh repo create you/devstrap-hub --private
devstrap hub init git@github.com:you/devstrap-hub.git
```

`hub init` accepts the git-carrier URI forms (`git+ssh://`, `git+https://`, `git+file://`,
scp-like `git@host:path.git`, optional `?branch=`) and writes it into `config.yaml`. There is
no bucket and no new credential plane — auth is your existing SSH key / credential helper. Git
runs **non-interactively**, so load your key with `ssh-add ~/.ssh/<key>` first; a missing or
denied key fails fast with an auth error and a hint rather than hanging on a prompt.

How it works: DevStrap keeps a local clone under `~/.devstrap/hub-git/<hash>/`, fetches and
hard-resets to the remote head on reads, and on writes applies content-addressed (never
colliding) file mutations, commits once per batch, and pushes. The atomic push-ref is the
compare-and-swap point; a non-fast-forward simply refetches and re-applies. No `git merge` ever
runs.

**Bound the history.** Deleting objects never shrinks a Git repo. Run `devstrap hub compact`
periodically: it publishes a snapshot, advances the retention floor, deletes cold events, and
force-pushes a squashed single-commit branch so the host can GC the unreachable history.

**Forge limits apply to the carrier.** GitHub, for instance, hard-limits objects to 100 MB and
pushes to roughly 2 GiB. Large env/draft blobs that exceed the per-object limit want an
S3-compatible hub instead.

### Folder carrier

Point the hub at a plain shared directory — a Dropbox/iCloud/Google Drive folder, or an
SMB/NFS mount:

```yaml
# ~/.devstrap/config.yaml
hub: "folder:/Users/you/Library/CloudStorage/Dropbox/devstrap-hub"
```

The path must be **absolute**. `hub init` is git-only, so set the folder scheme directly in
`config.yaml` or `DEVSTRAP_HUB`. The cloud drive (or network mount) is itself the replication
transport, so there is no fetch/commit/push loop — DevStrap writes ciphertext objects straight
into the folder and lets the drive sync them. The cross-process lock and each device's
observation floor stay in a **local** cache (`~/.devstrap/hub-folder/<hash>/`), never inside the
shared folder, so lock churn is never replicated.

One caveat: a cloud drive gives no cross-writer linearization point, so two devices writing the
same conditional object at the same instant can both "win" and the drive resolves it as a
conflicted copy. This is acceptable for the single-user, few-devices, rarely-simultaneous case
the folder carrier targets — ordinary convergence never collides because every object is
content-addressed or `(device, seq)`-unique.

### R2 / S3 carrier (scale)

For an S3-compatible bucket (Cloudflare R2, AWS S3, MinIO):

```bash
# ~/.devstrap/config.yaml
#   hub: r2://<bucket>
export DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
export DEVSTRAP_HUB_S3_REGION=auto
export DEVSTRAP_HUB_S3_ACCESS_KEY_ID=…       # falls back to AWS_ACCESS_KEY_ID
export DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=…   # falls back to AWS_SECRET_ACCESS_KEY
devstrap sync
```

Credentials resolve most-explicit-first: the `DEVSTRAP_HUB_S3_*` env/config values (either may
be a 1Password `op://` ref, resolved at sync time) → the `AWS_*` literals → a per-workspace OS
keychain slot written by `devstrap hub login` (with a `0600` file fallback under
`DEVSTRAP_NO_KEYCHAIN`). The keychain/`op://` path is the recommended custody on developer
machines; plaintext env is the CI/override fallback. Full bucket provisioning, credential
custody, and the multi-device runbook are in
[`../spec/19_CLOUD_PROVISIONING_GUIDE.md`](../spec/19_CLOUD_PROVISIONING_GUIDE.md).

## Operating a hub

These maintenance commands apply to every carrier (pass `--hub-file` only for the test backend;
otherwise they read the configured hub):

- **`devstrap hub compact`** — publishes a sealed full-state snapshot, advances the signed
  per-device retention floor, and deletes the now-cold events below it, so the log stays bounded
  and a fresh device bootstraps from the snapshot instead of replaying all history. Tombstones
  are GC'd only once every approved device has acked them. Use `--dry-run` to preview the floors
  and delete count, `--min-events N` to skip trivial compactions, `--keep-snapshots N` to retain
  extra snapshot objects. On the Git carrier this is also what lets the host reclaim disk.
- **`devstrap hub gc`** — reclaims unreferenced blobs. It pulls and applies the log first so the
  mark set is complete, **refuses to sweep on an incomplete view**, and keeps blobs younger than
  `--grace-window` (default 24h) even when unreferenced, to bound the push-before-event race.
- **`devstrap hub migrate-events`** — rewrites the event object layout when the storage keying
  changes between versions.

`hub compact`, `hub gc`, and `hub migrate-events` are the destructive passes; on cooperating
clients they are serialized by an advisory sweep lock so two of them can't interleave. The lock
is advisory — it protects cooperating clients, not a hostile writer.

### Carrier history was rewritten

For a Git carrier, this refusal means the configured branch disappeared or its fetched head is
not descended from the last head this device verified, and the new tree does not carry the
retention manifest a `devstrap hub compact` squash presents — either strictly advanced, or
byte-identical to the one this device last verified (the parentless squash reuses the pre-squash
manifest bytes). DevStrap stops before it can silently re-found the carrier from one device's
partial local backlog.

First run `devstrap status` and `devstrap sync` on another trusted, up-to-date device. If that
device also refuses, restore or recreate the carrier repository from the host's backup. If the
other device confirms that the current carrier is intentionally correct, it is safe to re-adopt
that confirmed head on the refusing device by removing only its carrier cache and syncing again:

```bash
rm -rf ~/.devstrap/hub-git/<hash>
devstrap sync
```

The actual cache is `~/.devstrap/hub-git/<hash>/` (with `repo/`, `repo.lock`, `observed.json`,
and `head.json` beneath it); the refusal prints its exact path. Removing it discards only the
local carrier clone and continuity record, not workspace state or the remote carrier. Do this
only after a trusted device has verified the remote: deletion deliberately opens a fresh TOFU
adoption for that carrier head.

Note that re-adoption does not re-upload history: each device's push watermark still records
what it already pushed, so events the rewind erased from the carrier are not re-sent. If you
accept a carrier known to be missing history rather than restoring the host's backup, run
`devstrap hub compact` from an up-to-date device afterwards — the sealed snapshot it publishes
covers pre-rewind state for any device that later bootstraps from the carrier.

## Zero-knowledge, restated

The hub cannot read your data. Confidentiality holds by construction: blobs are age-encrypted
client-side to the enrolled device set, event payloads are envelope-encrypted under a per-epoch
Workspace Content Key, and repo bytes never traverse the hub at all. What the carrier still owes
you is **integrity and availability** — use a private repo/bucket/folder with sane access
controls and back it up. The threat model spells out exactly what each carrier does and does not
defend against: [`../spec/15_SECURITY_THREAT_MODEL.md`](../spec/15_SECURITY_THREAT_MODEL.md).

## See also

- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — the two-plane hub in context.
- [`../spec/03_SYSTEM_ARCHITECTURE.md`](../spec/03_SYSTEM_ARCHITECTURE.md) — the canonical Hub
  backend design.
- [quickstart.md](quickstart.md) — first-run setup, including pairing a second device.
