# Quickstart

This walks the first-run loop end to end: set up a workspace, adopt the repos you already have,
point at a hub, and sync. Then it covers pairing a second device and the agent loop. Install
first — see [install.md](install.md).

Prefer not to install? Every command below also works as `go run ./cmd/devstrap <cmd> …`.

## 1. Initialize a managed workspace

```bash
devstrap init ~/Code --workspace-name personal
```

`~/Code` becomes your managed namespace — a real folder DevStrap keeps consistent across
machines. `init` mints a stable workspace id and this device's key identities.

## 2. Adopt the repos you already have

```bash
devstrap scan ~/Code --adopt
devstrap status
```

`scan --adopt` walks the tree, records each Git project (path, remote, default branch), warns
about secret-looking files, and prunes generated folders. `status` shows what DevStrap now
manages.

To add a new repo and materialize it in one step:

```bash
devstrap clone git@github.com:acme/api.git work/acme/api --open
```

## 3. Point at a hub (zero-infrastructure default)

The recommended hub is **any private Git repo you can already push to** — no bucket, no new
credential plane, zero infrastructure. Create an empty private repo and register it:

```bash
gh repo create you/devstrap-hub --private
devstrap hub init git@github.com:you/devstrap-hub.git
```

`hub init` writes the hub URI into `~/.devstrap/config.yaml` (`hub: "git+ssh://…"`). Auth is
your existing SSH key / credential helper, and Git runs non-interactively — load your key with
`ssh-add ~/.ssh/<key>` first. (For a shared cloud-drive folder or S3/R2 at scale, see
[self-hosting.md](self-hosting.md); for local testing without any remote,
`devstrap sync --hub-file /tmp/hub/events.json` works too.)

## 4. Sync and open

```bash
devstrap sync
devstrap open work/acme/api --cursor   # the repo cloned in step 2 — any managed path works
```

`sync` pushes your local namespace events, pulls anything new, and then **eagerly materializes
the tree**: it blobless-clones every repo from its existing remote, extracts encrypted
draft/env blobs, and hydrates env profiles. After it returns the folders are really on disk —
`open` just launches the editor.

Run `devstrap hub compact` periodically: deleting objects never shrinks a Git carrier, so
compact squashes cold history and lets the host reclaim it.

The full eight-line default loop:

```bash
devstrap init ~/Code --workspace-name personal
devstrap scan ~/Code --adopt
devstrap status
gh repo create you/devstrap-hub --private
devstrap hub init git@github.com:you/devstrap-hub.git
devstrap sync
devstrap open <any-managed-path> --cursor   # a path from `devstrap status`
devstrap run-loop        # optional: scan + sync + materialize on an interval, no daemon
```

## Pair a second device

Devices converge only when they share **one** workspace id. The founder mints it; every later
device adopts it. Three commands make the two-device ceremony one guided path each side:
`devstrap up` bootstraps the founder, `devstrap pair` runs the founder's half of the ceremony, and
`devstrap join` is the joiner's single command. The founder's pairing code is a `devstrap-pair2:`
blob that carries the founder's fingerprint **and** (for a remote hub) the hub URI. Reading the
fingerprint aloud is **optional high-assurance**: the embedded value trusts your paste channel
(1Password/Slack/iMessage); pass `--fingerprint` to also compare it out-of-band, which is the only
thing that defends a *compromised* paste channel. A carried `file:`/`folder:` hub is never
auto-applied — the blob is unauthenticated, so a local filesystem target always needs a manual
`devstrap hub init` to confirm it.

```bash
# Founder — one command bootstraps the workspace: init + scan + hub + sync
devstrap up ~/Code --hub git@github.com:you/devstrap-hub.git

# Founder — the guided wizard: it prints your code + the exact command for the
# second device, then blocks until you paste the code that device prints back,
# and automatically approves it and publishes the key grant.
devstrap pair
#   → prints: devstrap-pair2:… and "On the SECOND device, run: devstrap join '<code>'"
#   → then paste the joiner's code when prompted; pair enrolls + approves + syncs

# Joiner — one command: adopt the id, pin the founder, configure the hub, and
# print this device's own code (paste it back into the founder's `pair` prompt)
devstrap join '<founder-code>'
# High-assurance variant (verify the fingerprint out-of-band against the founder's stderr):
#   devstrap join '<founder-code>' --fingerprint <founder-fingerprint>

# Joiner — sync once more to receive the grant; the whole tree materializes
devstrap sync
```

`devstrap up` folds `init` (+ `scan --adopt`) + hub configuration + `sync` (each step is
independently idempotent, so a re-run resumes from any interruption). `devstrap pair` folds the
founder side of `devices pairing-code` + `devices enroll --code --approve` + `sync`; it needs an
interactive terminal (in a script/CI use the manual flow below). `devstrap join` folds
`init --join --code` + `hub init` + generating the joiner's own code — if the founder's code
carried no hub (or a local `file:`/`folder:` one), `join` says so and you run `devstrap hub init
<url>` yourself before the first `sync`.

The manual step-by-step ceremony remains the documented fallback (for scripts, CI, recovery, and
v1 codes):

```bash
# Founder
devstrap sync                                       # founds the workspace, pushes the namespace map
devstrap devices pairing-code                       # stdout: devstrap-pair2:… (fingerprint + hub embedded)

# Joiner
devstrap init ~/Code --join --code '<founder-code>' --fingerprint <founder-fingerprint>
devstrap hub init <url>                             # only if the code carried no remote hub
devstrap devices pairing-code                       # this device's own code + fingerprint

# Founder — approve the joiner, then push the key grants
devstrap devices enroll --code '<joiner-code>' --approve --fingerprint <joiner-fingerprint>
devstrap sync

# Joiner — sync once more; the whole tree materializes
devstrap sync
```

The workspace key rotates automatically during `sync` once its epoch ages past
`keys.rotate_max_age` (default 90 days); `devstrap keys rotate` forces it, and
`devstrap devices revoke` is the response to a known key compromise. The full pairing runbook,
including fleets larger than two devices and wedge recovery, is in
[`../spec/19_CLOUD_PROVISIONING_GUIDE.md`](../spec/19_CLOUD_PROVISIONING_GUIDE.md) §E.

## The agent loop

DevStrap runs agent tasks in fresh, isolated worktrees off the fetched remote default branch —
never a stale local branch — and records the base SHA so a PR can't be opened against a moved
base.

```bash
# Fresh worktree off origin/<default_branch>
devstrap worktree new work/acme/api --fresh-upstream --name fix-tests

# Run an agent (explicit argv) in that worktree; output logged, run recorded
devstrap agent run work/acme/api --engine generic --task "run tests" -- npm test

# Open a PR/MR once the run is complete (base-gated; --dry-run to preview)
devstrap agent pr <run-id> --dry-run
```

`agent run` wraps the child in an OS-enforced sandbox by default (`--sandbox auto|off|require`):
`auto` is best-effort — it warns and retains today's advisory-only behavior when no supported
backend is available; `require` refuses to run unsandboxed; `off` disables the sandbox entirely.
When available: macOS Seatbelt, and on Linux bubblewrap falling back to Landlock+seccomp where
user namespaces are restricted. The wrapper's command/file policy is guardrails layered beneath
that sandbox, not a full sandbox itself — see
[`../spec/10_AGENT_WORKSPACES_AND_POLICIES.md`](../spec/10_AGENT_WORKSPACES_AND_POLICIES.md).

## Where to next

- Full command list: `devstrap <command> --help`, or the command reference in the
  [README](../README.md#command-reference).
- Choosing and operating a hub: [self-hosting.md](self-hosting.md).
- The big picture: [`../ARCHITECTURE.md`](../ARCHITECTURE.md).
