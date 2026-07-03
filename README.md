<div align="center">

<img src="repo_image2.png" alt="DevStrap — managed code namespaces for developers" width="860">

<h1>DevStrap</h1>

<strong>Your code. Your structure. Always in sync.</strong>

<p>
A local-first <em>Workspace Passport</em>: one identical <code>~/Code</code> namespace on every machine and AI agent —
built on Git, SQLite, and age‑encrypted secrets, <em>not</em> a magic filesystem.
</p>

<p>
<a href="https://github.com/Reederey87/DevStrap/actions/workflows/ci.yml"><img src="https://github.com/Reederey87/DevStrap/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
<a href="https://goreportcard.com/report/github.com/Reederey87/DevStrap"><img src="https://goreportcard.com/badge/github.com/Reederey87/DevStrap" alt="Go Report Card"></a>
<img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26">
<img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux-555" alt="macOS | Linux">
<a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="License: MIT"></a>
<img src="https://img.shields.io/badge/status-alpha-orange" alt="Status: alpha">
</p>

</div>

---

## What is DevStrap?

DevStrap gives you a **portable, managed code namespace** — the *Workspace Passport* — that appears identically on every device you work from: your Mac, a Linux box, a cloud VM, or an AI agent runner.

The idea is deliberately boring and robust: `~/Code` is a **real folder**, and DevStrap keeps its structure consistent everywhere using developer‑native tools underneath — **not** a FUSE/virtual filesystem.

- **Git** owns repository contents (cloned on demand, `--filter=blob:none`).
- **SQLite** owns the local namespace map and workspace state.
- **Secrets** are referenced (1Password) or **age‑encrypted**, never blindly copied.
- **Agents** always start from a **fresh worktree off the fetched remote default branch** — never a stale local branch.

> **Install DevStrap on a new machine → point it at `~/Code` → authenticate Git + secrets → run `devstrap sync` once → the whole tree is reconstructed.** Every repo is blobless‑cloned from its existing remote, env/draft folders are pulled as encrypted blobs, and `node_modules`/build artifacts are rebuilt, never synced.

## Table of contents

- [Why DevStrap?](#why-devstrap)
- [How it works](#how-it-works)
- [Features](#features)
- [Project status](#project-status)
- [Requirements](#requirements)
- [Install](#install)
- [Quickstart](#quickstart)
- [Command reference](#command-reference)
- [Architecture](#architecture)
- [Roadmap](#roadmap)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

## Why DevStrap?

Moving between machines and handing work to AI agents breaks in predictable ways:

- Your `~/Code` layout drifts from one machine to the next.
- Repos are cloned ad‑hoc into inconsistent paths.
- `.env` files get copied around in plaintext (or lost).
- "I forgot to push" strands work on the wrong box.
- Agents branch from a **stale local `main`** and open PRs against the wrong base.

DevStrap fixes these without a heavyweight sync daemon or a virtual filesystem. It treats your code namespace as **managed state** — a signed, append‑only event log of *where every project lives, what its remote is, and which env profile it uses* — and reconstructs the real tree from that map plus Git's own transport.

## How it works

File‑sync is **split by content type** — DevStrap never blanket‑syncs a folder, and never file‑syncs `.git` (which would corrupt the repo):

| Content | Transport |
|---|---|
| **Repo content** | `git clone --filter=blob:none` / fetch from its existing remote — rides Git's transport, never touches the hub |
| **Env vars + non‑git/draft folders** | age‑encrypted, content‑addressed `age_blob:<sha256>` bundles |
| **The map of all projects** | a signed, [HLC](https://cse.buffalo.edu/tech-reports/2014-04.pdf)‑ordered append‑only event log (the "namespace map") |
| **`node_modules` / build artifacts** | **never synced** — rebuilt on hydrate |

Materialization is **eager**: after `devstrap sync`, the whole `~/Code` tree is really present on disk. There is no placeholder/lazy‑VFS magic — a true virtual filesystem (StrapFS) is explicitly deferred.

```text
1. Add or create a project on Machine A.
2. DevStrap records it in the signed namespace map (path, remote, env profile, policy).
3. Machine B runs `devstrap sync` and pulls the map.
4. Sync eagerly materializes the tree: blobless-clone each repo, pull encrypted env/draft blobs, hydrate env.
5. The same folder paths are really present on disk.
6. Agent work starts from a fresh remote default branch — not a stale local one.
```

## Features

- 🗂️ **Real managed namespace** under `~/Code` — owned structure + metadata, not a mounted illusion.
- 💧 **Repo hydration & skeleton directories** — projects exist as lightweight skeletons until materialized; `sync`/`materialize` blobless‑clone them eagerly.
- 🔄 **Git freshness** — partial clone, LFS policy, authoritative default‑branch resolution, stale‑base detection.
- 🔐 **Secrets mapping** — repo‑specific env profiles, age‑encrypted at rest or referenced from 1Password; subprocesses get a sanitized, no‑secret‑leak environment.
- 🤖 **Agent worktrees** — every agent task runs in an isolated worktree off the fetched remote default branch, with a wrapper‑level command/file policy and forge‑aware PR/MR creation (`gh`/`glab`/`tea`).
- 🧰 **Mac‑first, Linux‑compatible** — one portable Go binary; platform behavior sits behind adapters.
- 🛰️ **Zero‑knowledge sync hub** — a two‑plane hub (signed event log + content‑addressed encrypted blob store) on Cloudflare R2/S3, behind one pluggable `Hub` interface.

## Project status

> **Alpha.** The local engine and the agent loop are shipped and tested; the cloud‑sync layer has landed and the R2/S3 hub backend is shipped — point `hub: r2://<bucket>` at a bucket, supply `DEVSTRAP_HUB_S3_*` credentials, and `devstrap sync` talks to it.

**Shipped**

- Phase 0 local CLI: `init`, `scan`/`add`/`hydrate`/`open`, `worktree`, `env`, `run`, `status`, `doctor`, `db`, `devices`, `conflicts`.
- Phase 3 agent loop: fresh‑worktree `agent run`, recorded logs, base‑gated `agent pr` with forge‑aware routing.
- Cloud‑sync workstreams (PR #16): **eager materialization** (`sync`/`materialize`), **encrypted draft bundles** + `.devstrapignore` compiler (`draft`), the **pluggable `Hub` interface + R2/S3 backend**, and a **portable `run-loop`** (scan + sync + materialize on an interval, no daemon).
- Hardened internals: sanitized child env, value‑level secret redaction, partial clone with retry classification, WAL SQLite with single‑writer pool, HLC event ordering with conflict reconciliation, age X25519 device identities in the OS keychain (file‑store fallback for headless/CI).

**Not yet implemented**

- The local daemon, FSEvents‑specific Mac watcher, and native LaunchAgent/systemd installers.
- The **hosted** control plane: production remote device enrollment and out‑of‑band fingerprint confirmation (the R2/S3 hub backend itself is shipped).
- OS‑enforced agent sandboxing (today's command/file policy is wrapper‑level).

A standing design/implementation audit drives the backlog. All passes are archived under [`docs/audits/`](docs/audits/) — see the [index & open backlog](docs/audits/README.md). The latest is the sixth pass, [`AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`](docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md) (43 findings, building on the 36-finding fifth pass).

## Requirements

- **macOS or Linux**
- **Go 1.26+** (to build from source)
- **Git**
- **GitHub CLI (`gh`)** — and optionally `glab`/`tea` — for PR/MR creation

Optional:

- **1Password CLI (`op`)** for secret‑provider mode (`env bind` / `run`).
- **Cursor** or **VS Code** command‑line launchers for `devstrap open`.

## Install

### Download a release binary

Prebuilt binaries for macOS and Linux are published on the [Releases](https://github.com/Reederey87/DevStrap/releases) page (built via GoReleaser). Download, extract, and put `devstrap` on your `PATH`.

```bash
# example: install a downloaded release binary into ~/.local/bin
install -m 0755 ./devstrap ~/.local/bin/devstrap
devstrap version
```

### Build from source

```bash
git clone git@github.com:Reederey87/DevStrap.git
cd DevStrap
go build -o bin/devstrap ./cmd/devstrap
./bin/devstrap version
```

> A Homebrew tap and a `curl | sh` installer are on the roadmap (audit `PROD-05`). Until then, use a release binary or build from source.

## Quickstart

```bash
# 1. Initialize a managed workspace at ~/Code
devstrap init ~/Code --workspace-name personal

# 2. Adopt the repos you already have on disk
devstrap scan ~/Code --adopt
devstrap status

# 3. Add a new repo and materialize it in one command
devstrap clone git@github.com:acme/api.git work/acme/api --open
# (or the explicit two-step form: devstrap add … then devstrap hydrate …)

# 4. Capture and re-hydrate its environment (encrypted at rest)
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local

# 5. Start agent work from a fresh remote default branch
devstrap worktree new work/acme/api --fresh-upstream --name fix-tests
devstrap agent run work/acme/api --engine generic --task "run tests" -- npm test
devstrap agent pr <run-id> --dry-run

# 6. Point at a hub, then sync the namespace map + materialize the tree.
#    Configure the hub once in ~/.devstrap/config.yaml:
#      hub: r2://<bucket>
#    and supply S3/R2 credentials via the environment:
#      export DEVSTRAP_HUB_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
#      export DEVSTRAP_HUB_S3_ACCESS_KEY_ID=…   # falls back to AWS_ACCESS_KEY_ID
#      export DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=…  # falls back to AWS_SECRET_ACCESS_KEY
#    (credentials can also be stored via `devstrap hub login` or a 1Password `op://` ref).
#    See spec/19_CLOUD_PROVISIONING_GUIDE.md for the full R2 setup.
devstrap sync

# For local testing without a bucket, a file-backed hub still works:
#   devstrap sync --hub-file /tmp/devstrap-hub/events.json
```

Prefer not to install? Every command also works via `go run ./cmd/devstrap <cmd> …`.

### Pair a second device

The R2/S3 hub keys everything under `workspaces/<workspace_id>/`, so devices converge only when
they share **one** workspace id. The **founder** mints it at `init`; every later device
**adopts** it — a bare `devstrap init` mints a *fresh* id and keys a disjoint prefix, so it
never sees the founder's content. The workspace id is a non‑secret prefix selector (excluded
from event signatures); it is exchanged out‑of‑band alongside the founder's public keys, and
authorization comes from the key exchange, not the id.

Pairing is a **two‑paste ceremony** (founder code → joiner, joiner code → founder) plus one
out‑of‑band fingerprint read in each direction — the `devstrap-pair1:` code is non‑secret, but
the fingerprint (read aloud over a trusted channel) is what authorizes the keys.

```bash
# Founder — found the workspace and print the pairing code
devstrap init ~/Code
# set `hub: r2://<bucket>` in ~/.devstrap/config.yaml (+ DEVSTRAP_HUB_S3_ENDPOINT) — step 6 above
devstrap hub login                          # store the R2/S3 secret (do this AFTER init)
devstrap sync                               # founds the workspace, pushes the namespace map
devstrap devices pairing-code               # stdout: devstrap-pair1:...  stderr: founder fingerprint

# Joiner — adopt the id and pin the founder in one step, then log in to the hub
# (fleets >2 devices: pin every existing device the same way — unpinned signers'
#  events quarantine and replay once approved)
devstrap init ~/Code --join --code '<founder-code>' --fingerprint <founder-fingerprint>
devstrap hub login                          # AFTER the id-adopting init (the credential slot keys on the workspace id)
# joiner needs the same `hub: r2://<bucket>` config.yaml entry as the founder
devstrap devices pairing-code               # the joiner's own code + fingerprint, sent back to the founder

# Founder — approve the joiner in one command, then both sync
devstrap devices enroll --code '<joiner-code>' --approve --fingerprint <joiner-fingerprint>
devstrap sync                               # pushes the key grants

# Joiner — sync once more; the whole tree materializes
devstrap sync
```

The workspace key rotates automatically during `sync` once its active epoch is older than
`keys.rotate_max_age` (default 90 days); `devstrap keys rotate` forces it, and `devstrap
devices revoke` is the response to a *known* key compromise.

> The workspace id **cannot** be changed on an already‑initialized store — remove the DevStrap
> home (`~/.devstrap`) and re‑run `init --join --code`. This is safe: no repo content lives
> there. See [`spec/19_CLOUD_PROVISIONING_GUIDE.md`](spec/19_CLOUD_PROVISIONING_GUIDE.md) §E for
> the full runbook, including rotation cadence and wedge recovery.

## Command reference

| Command | Description |
|---|---|
| `devstrap init` | Initialize a DevStrap workspace |
| `devstrap status` | Show local workspace status (`--json` supported) |
| `devstrap doctor` | Check local prerequisites |
| `devstrap scan` | Scan a workspace root for projects (`--adopt`, `--quarantine`) |
| `devstrap clone` | Clone a repo into the namespace and materialize it in one command (`--open`/`--vscode`) |
| `devstrap add` | Add a Git repository to the namespace |
| `devstrap hydrate` | Clone a skeleton Git repository |
| `devstrap open` | Hydrate and open a namespace path in an editor (`--cursor`/`--code`) |
| `devstrap materialize` | Eagerly materialize skeleton projects (clone repos, hydrate env) |
| `devstrap sync` | Push/pull namespace events and materialize the tree (hub from config, e.g. `hub: r2://<bucket>`; `--hub-file <path>` overrides for local tests) |
| `devstrap run-loop` | Run scan + sync + materialize on an interval (portable, no daemon) |
| `devstrap worktree` | Manage isolated worktrees (`new`/`status`/`finalize`/`list`/`remove`/`cleanup`/`unlock`) |
| `devstrap agent` | Run agents in isolated fresh worktrees (`run`/`list`/`show`/`pr`) |
| `devstrap env` | Manage project environment profiles (`capture`/`hydrate`/`bind`/`rotate`) |
| `devstrap run` | Run a command with the project env profile injected |
| `devstrap draft` | Manage non‑git draft project content sync (`snapshot`) |
| `devstrap hub` | Operate on the sync hub (`gc` reclaims unreferenced blobs) |
| `devstrap devices` | Manage device trust state (`list`/`approve`/`revoke`/`lost`/`rename`) |
| `devstrap conflicts` | List open namespace conflicts |
| `devstrap db` | Manage the local state database (`migrate`/`status`/`backup`/`down`) |
| `devstrap version` | Print build version |

Run `devstrap <command> --help` for flags and subcommands.

## Architecture

DevStrap is a Mac‑first, Linux‑compatible **managed physical namespace** — not a virtual filesystem.

```text
~/Code                          user-visible managed tree (real folders)
~/.devstrap/state.db            local SQLite state (WAL, 0600)
~/.devstrap/blobs/              age-encrypted env/draft blobs (0600)
~/.devstrap/keys/               device identities (keychain preferred; file fallback)
~/.devstrap/worktrees/          managed agent/human worktrees
~/.devstrap/devstrapd.sock      future local daemon socket
```

Components:

- **`devstrap`** — the CLI for workspace setup, status, hydration, worktrees, env, sync, and agents (shipped).
- **`devstrapd`** — a local daemon for reconciliation, watchers, and a local API (planned).
- **DevStrap Hub** — a two‑plane zero‑knowledge sync service: a signed HLC namespace‑map event log plus a content‑addressed encrypted blob store, on Cloudflare R2/S3 behind one pluggable `Hub` interface (shipped; a hosted control plane for device enrollment is still planned).

The full design corpus lives under [`spec/`](spec/) — start with [`spec/00_START_HERE.md`](spec/00_START_HERE.md).

## Roadmap

Capability layers (see [`spec/14_MVP_ROADMAP_AND_BACKLOG.md`](spec/14_MVP_ROADMAP_AND_BACKLOG.md) for the canonical, re‑ordered sequencing):

1. **Local CLI proof** — scan, register, hydrate, fresh worktrees, env profiles. ✅
2. **Agent workspaces** — one worktree per task, fresh remote base, logs, forge‑agnostic PR/MR. ✅
3. **Multi‑device sync** — eager materialization, encrypted draft/env blobs, the R2 zero‑knowledge hub. 🚧
4. **Mac daemon** — LaunchAgent, FSEvents watcher, shell/editor integration. ⏳
5. **Optional StrapFS** — File Provider / FUSE evaluation. ⏳ (deliberately deferred)

The near‑term priorities — now that the R2/S3 hub backend is shipped behind the `hubFromOptions` seam — are to bound sync‑log growth (event‑log compaction + full‑state snapshot exchange and a retention marker), harden the hub's zero‑knowledge guarantees, and then grow the transport and product surface (an HTTP/SSE relay, production device enrollment, and a `service install` daemon). They are detailed across the [audit archive](docs/audits/) (latest: the sixth pass).

## Security

DevStrap is built so the sync hub is **zero‑knowledge**: repo content rides Git's own transport and never reaches the hub, and env/draft content is **age‑encrypted client‑side** into content‑addressed blobs. Device identities are age X25519 + Ed25519 keypairs kept in the OS keychain (with a `0600` file fallback for headless/CI), and secret values are redacted from logs, errors, and event payloads.

Please report vulnerabilities privately per [SECURITY.md](SECURITY.md). The threat model is documented in [`spec/15_SECURITY_THREAT_MODEL.md`](spec/15_SECURITY_THREAT_MODEL.md); known hardening gaps are tracked as `SEC-*` findings in the latest audit.

## Contributing

Contributions are welcome! Before changing behavior, read [`spec/00_START_HERE.md`](spec/00_START_HERE.md) and the relevant spec file, and follow the agent/maintainer guidance in [AGENTS.md](AGENTS.md).

DevStrap uses **trunk‑based development**: `main` is the single protected default branch (there is **no** `dev` branch). All changes land via pull request to `main`; external contributors fork and open a PR, maintainers branch from the fetched `origin/main`. Agents and worktrees always base from the fetched `origin/main`, never a local branch. `main` is protected — PRs require green CI (Spec drift, Go lint, Go tests on macOS + Linux, Vulnerability check), resolved conversations, and linear history.

Before opening a PR:

```bash
gofmt -w cmd internal
golangci-lint run
go run ./cmd/spec-drift --base origin/main --head HEAD
go test -race ./...
```

Keep changes aligned with the safety invariants: never overwrite dirty worktrees, never log secrets, keep Mac‑specific behavior behind adapters, and never branch agent work from a stale local default branch. Add focused tests for anything touching Git, secrets, filesystem reconciliation, or destructive actions. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## License

DevStrap is licensed under the [MIT License](LICENSE).

---

<div align="center">
<sub><img src="icon.png" alt="DevStrap app icon" width="56" align="middle">&nbsp; <strong>DevStrap</strong> — your code, your structure, always in sync.</sub>
</div>
