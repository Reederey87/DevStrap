# Recovering your workspace

DevStrap owns the shape of `~/Code`. The fair question — the one the threat model asks too —
is what happens to that namespace if DevStrap goes away, or if the machine holding its SQLite
store does.

There are two answers, and they are for different situations.

| You have | Use | Recovers |
|---|---|---|
| A DevStrap backup (`db backup --full`) | `devstrap db restore` | Everything: namespace, encrypted env blobs, device keys, config |
| Only a `workspace.yaml` manifest | `devstrap import --manifest` — **or any `vcstool` install** | The namespace map; git projects clone from their own remotes |

`db backup --full` is the complete answer, but it is a DevStrap-format artifact only DevStrap
can read. That is a backup, not an escape hatch. The manifest is the escape hatch: a plain-text
file a tool DevStrap does not control can consume.

## Export a manifest

```bash
devstrap export --manifest ~/workspace.yaml
```

Add `--pinned` to record each repository's **resolved commit SHA** instead of its branch name —
the equivalent of `vcs export --exact`. A branch name says "whatever is on `main` today"; a SHA
says what you actually had. Use `--pinned` when the manifest is a recovery artifact and the
plain form when it is a description of the workspace.

Keep the file somewhere that survives losing the machine: a password manager attachment,
another host, a private gist. It contains no secrets (see [What it does not carry](#what-it-does-not-carry)),
but it does list every project path and remote URL you work on, so treat it as you would a
`~/.gitconfig`. `export` writes it `0600`.

Exporting is cheap and idempotent. Running it from a cron job or after every `devstrap sync` is
a reasonable habit.

## What the file looks like

It is a [`vcstool`](https://github.com/dirk-thomas/vcstool) `.repos` file — root key
`repositories`, entries keyed by relative path, each `{type, url, version}` — plus a single
`devstrap` key holding DevStrap's own metadata. That layout is deliberate: `vcstool` reads only
`repositories` and only those three attributes inside each entry, so the `devstrap` key is
ignored by construction and DevStrap's schema can evolve without ever colliding with its parser.

```yaml
# DevStrap workspace manifest — vcstool ".repos" schema.
# ... (the emitted file explains its own scope and what it does not carry)
repositories:
  work/acme/api:
    type: git
    url: git@github.com:acme/api.git
    version: main
devstrap:
  schema_version: 1
  workspace_id: ws_01jz...
  exported_at: "2026-08-01T00:00:00Z"
  pinned: false
  projects:
    work/acme/api:
      type: git_repo
      default_branch: main
      lfs_policy: auto
      env_profile: true
    notes/scratch:
      type: draft_project
```

`schema_version` is a floor, not a description: every key documented at version *N* is still
present, with the same name and meaning, at every version ≥ *N*. New keys may appear without a
bump, so **a consumer must ignore keys it does not recognize**. `devstrap import` does, and so
does `vcstool`.

## Recover without DevStrap

This is the point of the format. Install `vcstool` (`pipx install vcstool`, `uv tool install
vcstool`, or `pip install vcstool`) and point it at the manifest:

```bash
mkdir -p ~/Code
vcs import ~/Code < ~/workspace.yaml
```

Every git project in the manifest is cloned to its recorded path, at its recorded branch or
pinned SHA. No DevStrap binary is involved.

**What this recovers, exactly.** The `git_repo` projects, and only those. Projects of type
`local_git`, `plain_folder` and `draft_project` have no clonable remote — there is no
`{url, version}` for `vcs import` to act on — so they appear under `devstrap.projects` and are
structurally invisible to `vcstool`. Their *content* lives in age-encrypted draft bundles on
your hub and is recoverable only through DevStrap. For most workspaces the git subset is the
bulk of the value, but it is not the whole tree, and the emitted manifest says so in its own
header rather than leaving you to discover it.

If you have no `vcstool` and no DevStrap, the file is still plain YAML: every path and URL you
need is in it, and `git clone <url> <path>` in a loop is a five-line shell script.

## Recover with DevStrap

`devstrap import` is a **registration** plane. It writes namespace rows and stops; the existing
`sync`/`materialize` pass does the cloning, so there is exactly one code path that ever creates
a checkout.

```bash
# On the recovered/new machine:
devstrap init ~/Code
devstrap import --manifest ~/workspace.yaml
devstrap sync                # or: devstrap materialize
```

To keep the *same* workspace identity — so the recovered device rejoins the fleet rather than
founding a second workspace — pass the manifest's `devstrap.workspace_id` to `init`:

```bash
devstrap init ~/Code --workspace-id ws_01jz...   # implies --join
```

Import warns on stderr when the manifest's workspace id differs from the local one. That is a
warning, not a refusal: importing another workspace's manifest into a fresh one is a legitimate
thing to do on purpose.

Import is idempotent. Re-running it reports projects as `already_present` and changes nothing.
It never overwrites a project that is already registered differently — a stale manifest must not
be able to silently rewrite a live remote — and reports that as a skip instead.

### Exit codes and machine use

Both commands are machine contract surfaces: stdout carries exactly one document, all warnings
and diagnostics go to stderr, and the exit code alone signals success.

| Exit | Meaning |
|---|---|
| `0` | Everything registered (or was already present) |
| `1` | **Partial** import — some entries were skipped; stderr names each one |
| `2` | The file is not a workspace manifest, or could not be read |

A partial import still registers everything it could. Skipped entries include non-git
repository types (`hg`, `svn`, `bzr` — DevStrap materializes git only), unsafe namespace paths,
and conflicts with an existing project.

## What it does not carry

**No encrypted content, by design.** Captured env profiles and draft-folder bundles are
age-encrypted to your devices' recipients. A plaintext manifest cannot carry them and must not
appear to. The manifest records `env_profile: true` to name *which* projects have a profile; it
never carries the profile.

To recover secrets you need either the device key material (`devstrap db backup --full` →
`devstrap db restore`) or another enrolled device in the workspace that can re-encrypt to your
new device after you pair it. See [`self-hosting.md`](self-hosting.md) for the hub side and
[`quickstart.md`](quickstart.md) for pairing.

## Importing someone else's `.repos` file

Because the format *is* the `vcstool` format, interop runs both ways. A `.repos` file with no
`devstrap` key at all — hand-written, or produced by `vcs export` — imports directly:

```bash
devstrap import --manifest ./their-workspace.repos
```

Each entry becomes a `git_repo` project, and its `version` is adopted as the default branch
(unless it is plainly a resolved commit id, which would break every later fetch if recorded as
a branch name).
