# DevStrap — Design & Implementation Audit (Ninth and Tenth Passes)

_Date: 2026-07-31 · Trunk audited: `a7938dd` → `8e2fd21`_

Two passes are recorded in one file because they were run on the same day for one reason: **Pass 8 declared that two of the four dimensions it commissioned never reported.** Pass 9 exists to cover those. Pass 10 then covered the largest surface no pass had reached at all.

That sequence is the point. Pass 8 could have listed its findings and said nothing about the silent reviewers — the document would have read as complete. Instead it recorded the gap, and the two passes that gap prompted found **five real defects**, three of them in the plane whose reviewer had gone quiet twice.

## Pass 9 — the working-state and daemon planes

### Findings

| ID | Sev | Finding | Status |
|---|---|---|---|
| `P9-DAEMON-01` | P2 | One unreadable directory disabled native watching **device-wide, permanently** | Shipped (#262) |
| `P9-DAEMON-02` | P2 | A watcher hint cost as much as the periodic tick it pre-empted, voiding the entry gate's own cost argument | Shipped (#262) |
| `P9-WIP-01` | P2 | An empty mirror sha silently disarmed the leased delete | Shipped (#265) |
| `P9-WIP-02` | P2 | `wip push` called an **untracked-only tree "clean"** | Shipped (#266) |
| `P9-WIP-04` | P3 | Four present-tense spec claims contradicted the shipped plane, two inside their own files | Shipped (#267) |
| `P9-WIP-05` | P2 | A `wip.gc_interval` typo silently stopped **off-site replication** | Shipped (#267) |
| `P9-WIP-06` | P2 | The GC deleted refs a sha-agnostic tombstone merely **hid** | Shipped (#267) |
| `P9-WIP-03` | P3 | The commit-age veto is defeatable by the ref owner's own wrong clock at capture | **Open** |

### The two worth reading

**`P9-WIP-02`** is the one that mattered most to a user. `git stash create` — the primitive the recovery plane captures with — does not include untracked files and has no `-u` form. A tree holding only new files produced no stash object, so `wip push` reported *"working tree is clean"*. A mixed tree pushed successfully while silently omitting every new file. **This is the "forgot to push" feature failing at exactly the case it exists for**, since a brand-new uncommitted file is the most common thing anyone forgets. Fixed by reporting honestly rather than capturing more: capturing untracked files requires mutating the worktree, which this plane exists not to do.

**`P9-DAEMON-02`** contradicted a claim the roadmap made. The Milestone 5 entry gate justified building a watcher by arguing hints "invert the trade — converge when something actually changed, so the fleet can hold a long, cheap safe-interval *and* low latency." But `runLoopTick` ran its full-workspace scan unconditionally, and the "cheap enough every tick" judgement behind that was made against a **5-minute** interval while the watcher's floor is **5 seconds**. The trade never inverted. `spec/14`'s entry-gate review is corrected in place, because the reasoning is what future waves cite.

### `P9-WIP-06` — a decision, recorded so it can be overturned deliberately

A sha-agnostic drop tombstone can bury a genuinely later push under HLC skew, after which the live ref looks like an orphan the GC reaps. The fix declines to touch the hiding and closes only the destruction leg, on an asymmetry that generalizes past this plane:

> A hidden row is **recoverable** — `wip fetch --device <id>` derives the ref canonically and ignores the mirror entirely. A deleted object is **not**. Where a visibility rule and a destruction rule disagree, a recovery system resolves toward the recoverable outcome.

The hiding stays because removing it resurrects the phantom rows the tombstone exists to clear, and an existing test pins that hiding as *intended* — changing it is a design change needing its own evidence.

## Pass 10 — hub carriers, blob plane, snapshot exchange

**Verdict: nothing above P3, and no P3 reported.** This is a clean result, not an empty one, and it is recorded in full because a clean pass is only useful if it says what was attacked.

Six concrete hypotheses were chased and refuted, each with reasoning so no later pass re-walks them:

1. **Git-carrier CAS losing a concurrent push during compaction's force-with-lease squash.** Every retry re-runs `refreshLocked` before re-deriving what to delete or write, so a lost lease always re-observes the winner's state first. No ordering drops or duplicates an event.
2. **A snapshot referencing a blob `hub gc` later deletes.** `RetainedBlobRefs` is local-DB-derived, but `BuildSnapshot` seals from current state at compact time, and any later ref change is itself an event above the floor — so a joiner recovering via `ErrSnapshotRequired` always replays the correcting event before materializing. `hub gc` never touches the event log.
3. **Ack-gated tombstone GC skipping a never-acked device.** Verified: it *refuses* (`ready=false`) rather than ignoring — fail-safe.
4. **Envelope/grant ingestion failing open, or quarantining forever.** Every branch is grace-bounded, and `ReplayUndecryptableConflicts` retries open quarantines each cycle, so a later grant always recovers.
5. **The WCK owed-rotation marker clearing without a real rotation.** It clears only after a local `Rotate()` succeeds, never on "newer epoch observed".
6. **The anti-rewind content gate scoped to event prefixes only.** Real, and verified precisely — but documented verbatim in `spec/15` as an accepted residual under the dumb-carrier posture, not a gap the design believed it closed.

Two of these (3 and 6) were independently spot-checked by the coordinator rather than accepted on report.

**Also checked clean:** the `fsLock` cross-process lock, `CompactEventsBelow` floor semantics, `hub gc`'s completeness gate and re-stat-before-delete, sweep-lock TTL staleness, grant idempotency and kid-collision handling, `workspacekeys.Keyring` including the legacy kid-less upgrade, snapshot trust projection, and `envbundle` encrypt/decrypt/rewrap. No vacuous tests found; the only `t.Skip`s are legitimate live-integration gates.

## What both passes say about method

Every finding marked shipped above was **reproduced by the coordinator before being accepted** — several by executing the defect rather than re-reading the code. One changed severity on that evidence. Two candidate fixes were **written and then reverted** because existing tests contradicted them and the tests were right.

Nine checks during this work looked green while proving nothing, including one in the verification tooling itself (`gofmt -l` lists unformatted files while exiting 0, so `&& echo clean` fired regardless). The reproductions are recorded in `spec/07`, `spec/08`, `spec/11`, and `spec/18`.

The generalizable rules those produced, now in the specs rather than only in commit messages:

- A safety mechanism with an "off" state reachable by omission is not a safety mechanism (`spec/07`).
- When a primitive's blind spot is invisible in its return value, the caller must distinguish the cases and the primitive must document it (`spec/08`).
- Prefer anchored ignore patterns; `git add` says nothing when it skips an ignored path, and `test -f` passes for an untracked file (`spec/11`).
- Where a visibility rule and a destruction rule disagree, resolve toward the recoverable outcome (`spec/07`).

## Open after both passes

- `P9-WIP-03` (P3) — the commit-age veto is defeatable by the ref owner's own wrong clock at capture. Left open deliberately: it is a self-harm class needing a multi-step clock fault, and unlike `P9-WIP-06` it has no correction that does not trade away the veto's independence from mirror state.
- `P8-ADOPT-07` (P3) — pre-`00032` rows holding unresolved path spellings; self-healing, and a migration rewriting historical paths risks more than the aliasing it removes.
- Pass 7's four commercial/hosted-tier rows — unchanged, business-gated.

**No P1 is open in any pass.**
