# Namespace and Sync Model

## Core abstraction

The core object is a **namespace entry**.

A namespace entry maps a stable relative path to an intention:

```text
work/nclh/foc-models → Git repo at git@github.com:org/foc-models.git
experiments/fs2      → encrypted draft project
personal/scripts     → plain managed folder
```

The path is the product.

## Namespace entry example

```yaml
id: prj_01jz8devstrapabc
path: work/nclh/foc-models
type: git_repo
remote: git@github.com:org/foc-models.git
default_branch: main
materialization_policy: lazy
env_profile: snowflake-dev
tooling_profile: python-uv-snowflake
agent_policy: guarded
ignore_profile: default-code
created_at: 2026-06-23T12:00:00Z
updated_at: 2026-06-23T12:00:00Z
```

## Project types

### `git_repo`

Normal managed Git project.

Content source:

```text
Git remote
```

DevStrap syncs:

```text
path, remote, branch, profiles, state
```

DevStrap does not sync:

```text
working tree bytes, .git internals, dependencies
```

### `draft_project`

Small project without remote Git yet.

Content source:

```text
encrypted DevStrap draft bundle
```

Use for:

- experiments;
- scratch tools;
- early prototypes.

Limits:

```text
100 MB default max
5,000 files default max
ignore rules always applied
no plaintext secret files
```

### `plain_folder`

Structure-only folder.

Use for:

- grouping folders;
- documentation buckets;
- local-only areas.

## Device state

Each device has local state for every namespace entry.

Example:

```yaml
device_id: dev_macmini_upstairs
path: work/nclh/foc-models
state: ready
local_path: /Users/artem/Code/work/nclh/foc-models
current_branch: main
last_fetch_sha: abc123
local_dirty: false
env_ready: true
tooling_ready: true
last_seen_at: 2026-06-23T12:03:00Z
```

## Materialization states

```text
skeleton      path exists, repo/draft not hydrated
hydrating     job in progress
available     files exist locally
current       Git remote fetched and branch current
ready         env/tooling validation passed
dirty         local uncommitted changes exist
conflicted    requires user decision
failed        last hydration/sync job failed
```

## Event log

DevStrap sync should use append-only events.

Event fields:

```json
{
  "event_id": "evt_01jz...",
  "workspace_id": "ws_01jz...",
  "device_id": "dev_macmini_upstairs",
  "seq": 42,
  "type": "project.added",
  "payload": {},
  "created_at": "2026-06-23T12:00:00Z",
  "signature": "optional-later"
}
```

Event types:

```text
workspace.created
device.registered
device.revoked
device.heartbeat
project.added
project.updated
project.renamed
project.deleted
project.restored
repo.remote.changed
env.profile.bound
tooling.profile.bound
agent.policy.bound
draft.snapshot.created
conflict.created
conflict.resolved
```

## Sync protocol

Each device maintains a cursor:

```text
last_event_seq_applied
```

Sync loop:

```text
1. push local queued events to Hub
2. pull remote events after cursor
3. verify/decrypt where needed
4. apply events to local SQLite
5. reconcile local filesystem
6. update device heartbeat
```

## Hub storage

Hub stores:

- append-only events;
- device records;
- encrypted env bundles;
- encrypted draft snapshots;
- sync cursors;
- conflict records.

Hub does not store:

- plaintext secrets;
- raw hydrated Git repos;
- dependency folders;
- private keys.

## Hub deployment options

### Option A — Home hub

Run `devstraphub` on Mac Mini or GMK Ubuntu box.

Pros:

- quick for personal use;
- private;
- good for home-lab workflow;
- can be backed up by NAS.

Cons:

- remote access setup needed;
- hub availability tied to home network unless exposed securely.

### Option B — VPS/cloud hub

Run small service on a VPS.

Pros:

- always available;
- easier for cloud agents;
- path to SaaS.

Cons:

- hosting/security burden.

### Option C — Object-store backend

Use encrypted event/blob files in object storage.

Pros:

- simple infrastructure;
- cheap;
- durable.

Cons:

- conflict handling and locking are harder;
- less real-time.

### Option D — Hidden Git backend

Use a private implementation repo for events/manifest.

Pros:

- very fast MVP;
- free remote transport;
- easy audit.

Cons:

- psychologically conflicts with the product promise;
- Git merge conflicts return;
- should not be the long-term user-facing model.

Recommendation:

```text
Phase 1: local-only SQLite.
Phase 2: home-hub HTTP event log.
Phase 3: hosted hub or object-store adapter.
```

## Conflict model

Conflict is a first-class state.

Do not auto-resolve dangerous cases.

### Conflict: same path different remote

Example:

```text
work/api → git@github.com:acme/api.git
work/api → git@github.com:personal/api.git
```

Resolution options:

```text
keep local
use remote
rename one project
mark one unmanaged
```

### Conflict: same remote multiple paths

Example:

```text
work/api → git@github.com:acme/api.git
work/acme/api → git@github.com:acme/api.git
```

Resolution options:

```text
choose canonical path
move local clone
leave duplicate unmanaged
```

### Conflict: delete vs dirty local

Rule:

```text
Never delete dirty local clone.
Move to quarantine or keep unmanaged.
```

## Delete semantics

Namespace deletion creates a tombstone.

```text
project.deleted event → skeleton removed on clean devices
hydrated dirty devices → mark pending_delete_conflict
hydrated clean devices → move to quarantine, then purge later
```

Quarantine default retention:

```text
30 days
```

## Rename semantics

Rename is metadata-first.

```text
project.renamed old_path new_path
```

On each device:

- if skeleton: rename folder;
- if hydrated clean: move folder;
- if hydrated dirty: mark conflict;
- if target path exists: mark conflict.

## Draft sync model

Draft project snapshot:

```text
scan draft folder
apply ignore rules
create tar stream
encrypt for approved devices
upload encrypted blob
emit draft.snapshot.created event
```

Restore:

```text
download encrypted blob
decrypt locally
extract to skeleton path
preserve metadata where possible
```

Draft conflict rule:

```text
If two devices modify the same draft offline, create two snapshots and require manual merge.
```

## Namespace snapshot export

Support disaster recovery:

```bash
devstrap export --output devstrap-workspace-20260623.tar.age
```

Contains:

- namespace entries;
- device records;
- profiles;
- ignore rules;
- encrypted env bundles if requested;
- draft snapshots if requested.

