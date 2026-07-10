---
last_reviewed: 2026-07-10
tracks_code: [README.md]
---
# DevStrap — Commercialization & Pricing


DevStrap is an open-source CLI. This document sets out how a commercial offering can sit alongside the open-source project without compromising it: what stays free forever, what a paid managed tier adds, what it should cost, and what has to be built before any of it can ship. It is a plan, not a shipped feature — nothing here is built yet, and the engineering prerequisites (a control plane, a credential broker, metering) are called out explicitly so the plan is not mistaken for a roadmap that is already underway.

The guiding principle is the one every durable open-core developer tool converges on: **never gate the core capability behind payment.** The CLI, the sync engine, and self-hosting your own hub stay free and open-source forever. The paid tier sells operational convenience — a managed hub you don't have to run — not access to the product.

## 1. Positioning

DevStrap's core concept is the **Workspace Passport** (see `spec/adr/0001-product-naming.md`): a portable, managed `~/Code` namespace that reconstructs identically on every device from a hub the user provides. Today that hub is either a private git repository (zero infrastructure — the default) or the user's own Cloudflare R2 / S3 bucket. Both are self-provisioned: the user owns the storage, holds the keys, and the project earns nothing from their use. That is the open-source product and it is complete on its own.

The commercial opportunity is the segment of users who want the Workspace Passport but do **not** want to provision and operate a hub — create a bucket, manage credentials, run `hub compact` on a schedule, watch retention floors. For them, a **DevStrap-operated managed hub** removes the setup and the operations while preserving the zero-knowledge guarantee: the operator stores ciphertext it cannot read, exactly as R2 does today.

This is the Tailscale shape (free personal tier + managed paid tier + self-hostable Headscale coexisting) rather than the Docker shape (a tool that became contentious by tightening limits on an established free tier). The BYO-hub option is a **trust asset**, not a crippled trial — it is the answer to "what happens if you go away or change the pricing," and it should be featured in the managed-tier pitch, not hidden.

## 2. Tier model and the open-core boundary

Three ways to run DevStrap, two of them free forever:

| | **OSS — BYO git carrier** | **OSS — BYO R2/S3** | **Managed hub (commercial)** |
|---|---|---|---|
| Hub | Your private git repo | Your Cloudflare R2 / S3 bucket | DevStrap-operated bucket |
| Setup | `hub init <git-url>` | Provision bucket + creds | One command, no bucket |
| Operations | You run `hub compact`/`gc` | You run `hub compact`/`gc` | Operated for you |
| Cost | Free (your git host) | Free (your R2 bill) | Free tier + subscription |
| Keys | Yours | Yours | Yours (operator can't decrypt) |

**The open-core line — what never goes paid:**

- The `devstrap` CLI in its entirety: init, scan, sync, materialize, worktrees, agent runner, env capture/hydrate, device pairing, `keys rotate`, `db backup`, all of it.
- The sync engine, the encryption, the device-trust plane, the OS sandbox.
- **Self-hosting a hub** — both carriers. A user must always be able to point DevStrap at their own git repo or bucket and get the full product with zero payment and zero feature loss.
- The data format and protocol: open, documented, and never a lock-in lever. A managed-tier user must be able to `hub migrate-events` their workspace to a self-hosted hub and walk away.

**What the managed tier sells (operational, not capability):**

- A hub with no bucket to provision and no credentials to manage.
- Operated lifecycle: server-side compaction cadence, retention, and GC handled for the user (this needs new engineering — see §4 and finding `P7-PROD-02`).
- Fleet conveniences a solo user doesn't need but a team does: a device/usage dashboard, retention SLAs, audit-log export, and eventually org/SSO (most of this is unbuilt — §5).
- Support with a response commitment.

This boundary is the same one Coder draws (Community edition fully functional and self-hostable; paid tier is governance and scale only) and the one Infisical draws (MIT core, enterprise features in a separately-licensed module). It is deliberately **not** the Doppler/1Password boundary, where security basics like SSO sit behind a per-seat wall — that model is wrong for a solo-developer-first tool.

## 3. What comparable products charge, and the lesson from each

Researched 2026-07-10 from each vendor's live pricing pages. Prices are "as of" that date and move; the *models* are the durable signal.

| Product | Model | Free tier | Paid entry | Lesson for DevStrap |
|---|---|---|---|---|
| **Tailscale** | Per-user/mo | 6 users, free forever | $8/user (Standard) | The closest analog. Killed its confusing middle "Personal Plus" SKU (Pricing v4, 2026-04) and folded the limit into free; moved business plans off usage-based to *seat-based* because customers wanted predictable bills. Publish an explicit, friendly BYO-hub coexistence stance. |
| **Ngrok** | Flat base + usage credit + metered overage | 3 endpoints, 1GB, interstitial page | $8 (Hobbyist) / $20 (PAYG) | The free-tier interstitial is a cheap, non-blocking conversion nudge. But mixing hard credit-caps and metered overage in one ladder reads as three pricing models stacked — don't. No self-host option is Ngrok's weakness and DevStrap's differentiator. |
| **Docker** | Per-seat + company-size gate | Free <250 employees & <$10M rev | $9 (Pro) / $24/seat (Business) | Cautionary tale: announced free-tier pull caps + storage billing (Nov 2024) → "bait and switch" backlash → partial reversal (Feb 2025). The *price* rises stuck; the *usage-restriction* cuts had to be walked back. Never tighten an established free tier. Size-based free gate (free for small teams) is a clean idea. |
| **Doppler** | Per-human-seat | 3 users | $21/seat (Team) | "No fees for machine identities" is smart — DevStrap's agent runs/worktrees must never be a metered unit. But walling RBAC/SSO/MFA behind $21/seat is the wrong instinct for a solo-first tool. No self-host is a recurring complaint DevStrap's BYO hub answers. |
| **Infisical** | Per-identity (human+machine) | 5 identities | $18/identity (Pro) | The open-core template: MIT core, enterprise features in a license-gated `ee/` module enforced by a `LICENSE_KEY` with an offline path. Copy the boundary. Avoid the identity-counting billing (a small team with a few bots hits the wall immediately). |
| **1Password** | Per-seat | None (trial only) | $19.95/mo flat (Teams Starter, ≤10) | Trust-reputation anchor, not a pricing template — earns per-seat premiums a young tool can't. The flat-rate ≤10-seat "Starter Pack" is a proven micro-team wedge worth copying as an entry paid tier. |
| **Raycast** | Freemium + per-seat teams | Full core free, 50 AI msgs | $8/mo (Pro) | The most direct shape match: **free keeps the daily-driver whole; the paywall is cross-device *cloud sync* + usage scaling.** That is exactly DevStrap (local CLI free; a hosted hub is the paid axis). $8–10/mo is the individual-developer QoL ceiling. Stable price since 2023 = goodwill. |
| **GitHub Codespaces** | Usage-metered | 120 core-hrs + 15GB/mo (personal) | Seat fee excludes usage quota | Storage priced standalone and cheap ($0.07/GB-mo) is the market anchor for durable state storage. Seat-fee-≠-usage-allowance is a clean split. Never gates capability, only usage → stays "the default." |
| **Gitpod → Ona** | Bespoke usage credit (OCU) | 3 small envs, one-time credit | from $20/mo, ≤100 members no seat fee | Pivoted from commodity compute pricing to a value-unit — but the bespoke OCU drew bill-shock/opacity criticism. Zero-seat-fee pooled credits ≤100 members is a strong land-and-expand. **Avoid bespoke abstract billing units**; legible $/GB or $/seat wins. |
| **Coder** | Open-core, per-user/yr, sales-led | Community edition fully functional | ~$1,200/yr enterprise SKU | The open-core boundary to copy: core self-hostable and uncapped; paid = governance/scale (multi-org, audit, HA, SSO). But opaque sales-led pricing is wrong for DevStrap's self-serve stage — publish rate cards; reserve custom pricing for a real future enterprise tier. |

Cross-cutting lessons that decide DevStrap's model:

1. **Seat/flat over usage-metered for the paid tier.** Tailscale moved *away* from usage billing because companies want predictable bills; Ona's bespoke usage unit drew bill-shock complaints. DevStrap's own cost structure agrees (§4): hub *operations* are nearly free, so there is nothing worth metering by the operation.
2. **Free tier keeps the core whole; the paywall is cross-device convenience + scale.** This is the Raycast/Codespaces pattern and it maps onto DevStrap exactly — the CLI and BYO-hub are the whole product; the managed hub is the convenience.
3. **Never cut an established free tier.** Docker, Ngrok, and HashiCorp/Terraform all paid in goodwill for free-tier cuts. Because R2 is cheap and egress-free (§4), DevStrap can afford a genuinely generous free tier and should start generous rather than start stingy and loosen later — the opposite of the mistake that generates backlash.
4. **The BYO-hub escape hatch is a feature.** Every product above that lacked a self-host answer (Docker, Ngrok, Doppler) left its unhappiest users with nowhere to go. DevStrap's BYO hub is the credibility that makes the managed tier safe to adopt.

## 4. Managed-tier cost model — why the free tier is cheap to give

The P7-HUB dimension of the 2026-07-10 audit measured the R2 operation profile of a sync cycle directly from the adapter code. Cloudflare R2 pricing (2026-07-10): storage $0.015/GB-month; Class A (writes/lists) $4.50/million; Class B (reads) $0.36/million; **egress $0**.

Per sync cycle for a device: roughly `D + M + B + 3` Class A operations (device-stream lists + event puts + blob puts + ack) and `1 + N + blobs` Class B. An idle `run-loop` tick at a 60-second interval with three devices costs on the order of **$0.001 per device per month** in operations. Because R2 charges nothing for egress, transfer is free. **Storage (GB-month of uncompacted event ciphertext plus encrypted blobs) is the only cost that scales with use, and operations are financially negligible.**

Two consequences for pricing:

- **Meter storage, not operations.** There is no economic reason to bill per sync or per operation, and every reason not to (it invites the metering complexity users dislike). A free tier bounded by a storage cap and a device cap is both cheap to offer and legible to the user.
- **Compaction cadence is a real operator cost lever.** Uncompacted logs grow storage; the managed operator running compaction on the user's behalf (which requires new engineering — the operator holds no keys, see `P7-PROD-02`) directly controls the storage bill. This is operational value the free BYO-hub user provides for themselves and the managed user pays to have done.

What is **meterable today, without new code** (from the P7-PROD analysis): **storage bytes per workspace** (sum object sizes under the workspace prefix via a LIST loop) and **device count** (distinct device prefixes / ack objects). Both are visible to an operator with bucket access, need no client cooperation, and require no decryption. What is **not** meterable today and needs the control plane (§5): the mapping from a paying account to its workspace(s), and monthly-active-user counts by human.

## 5. Recommended packaging and price points

Anchored to the comparables (§3) and the cost model (§4). Metering unit: **per workspace, with a device cap and a storage cap** — the two dimensions that are meterable today and that map to how the product is actually used. Not per-seat-per-human at the entry tiers (a solo developer with five devices is one customer, not five), and never per-agent-run or per-sync.

| Tier | Price (target) | Who | Limits / what's included |
|---|---|---|---|
| **Free** | $0 forever | Solo devs, evaluators | Managed hub, 1 workspace, up to ~3 devices, a storage cap generous enough for real use (e.g. 2–5 GB of ciphertext), community support. Plus: unlimited everything on a **self-hosted** hub, always. |
| **Individual (Pro)** | ~$8/mo ($80/yr) | Solo devs who live in it | 1 workspace, unlimited devices, a larger storage cap (e.g. 50 GB), operated compaction/retention, email support. The Raycast/$8 anchor. |
| **Team** | flat ~$25/mo for a small team, then per-workspace | Small teams | Shared workspaces, higher storage, device/usage dashboard, audit-log export, priority support. A flat entry block (the 1Password "Starter Pack" wedge) before per-seat/per-workspace scaling. |
| **Enterprise** | Custom | Companies | SSO/SCIM, org roles, retention SLAs, custom terms. Sales-led, and genuinely future (needs §5 control plane + org concept). |

Notes on the numbers:

- **These price points are provisional** and depend on real waitlist demand (the website plan, `spec/21`, captures it) and eventual COGS once the control plane exists. The *structure* (generous free tier, ~$8 individual, flat-then-scale team, custom enterprise) is the recommendation; the exact figures are a starting hypothesis to test, not a commitment.
- **Trial policy:** no trial needed on Free — Free *is* the trial, and a real free tier converts better than a time-boxed one for a tool users adopt into a daily workflow. Offer a straightforward 14-day Pro trial for teams evaluating the paid conveniences.
- **A visible, non-blocking free-tier nudge** (Ngrok's insight, minus the annoyance): a one-line note in `sync` output on the managed free tier ("managed hub · free tier · N% of storage used") is honest, creates upgrade awareness, and never breaks anything.

### Engineering prerequisites before any of this can ship

The managed tier is **not** shippable on the current architecture. The audit's P7-PROD findings make the blockers concrete, and they are the real backlog for a hosted offering:

- **A control plane (`P7-PROD-04`, extends `P4-SEC-08`).** `workspace_id` is client-minted with no binding to an account. Billing, quotas, and device caps all require an authenticated account service mapping account → workspace(s) → devices → billing state. This is non-secret control metadata (a small Postgres, per `spec/19`); the data plane stays zero-knowledge.
- **A credential broker (`P7-PROD-04`, `P4-SEC-08`).** Shared bucket credentials cannot enforce per-tenant isolation or quotas. An STS-style broker issuing short-lived, prefix-scoped credentials is the enforcement point for both.
- **Server-side lifecycle + quota enforcement (`P7-PROD-02`, extends `P4-HUB-15`).** The operator holds no keys, so it cannot run the client-driven `hub compact`. Storage-layer lifecycle rules and LIST-based quota checks (object sizes are visible without decryption) are how a hosted operator bounds a tenant's footprint.
- **A version-skew policy (`P7-PROD-03`).** Fail-closed snapshot/envelope checks mean a mixed-version fleet after independent `brew upgrade`s can wedge. A paid product needs a documented N-1 compatibility guarantee and an in-CLI "device X needs an upgrade" signal before it can promise reliability.
- **Opt-in telemetry, or server-derived metrics (`P7-PROD-05`, extends `P4-HUB-14`).** No usage signal exists. Prefer deriving active-device/active-workspace counts from the credential broker's access logs (zero data-plane visibility) over client telemetry for anything billing-critical.

Until these exist, the managed tier is a plan and a waitlist, not a product. That sequencing is a feature of this document: it keeps the OSS project honest about what is shipped.

## 6. Licensing and open-core mechanics

- **Current state:** the repository is MIT-licensed (`LICENSE`). MIT is right for maximum adoption of the CLI and keeps the BYO-hub promise unambiguous.
- **Open-core boundary (if/when enterprise features are built):** follow the Infisical pattern — keep the core MIT, and if closed governance features (org/SSO/audit-console) are ever built, place them in a separately-licensed module rather than relicensing the core. Never rug-pull the core license; the Terraform/BUSL backlash and the OpenTofu fork are the cautionary tale, and for a trust-selling security tool the reputational cost would be existential.
- **Trademark:** "DevStrap" has no USPTO registration but real name-collision noise (several small OSS repos, `devstrap.net`/`devstrap.xyz` in use). A lightweight trademark search is worth doing before any paid marketing spend; the `spec/21` website plan covers the naming/SEO mitigation. Reserve the trademark question for when there is a business to protect.
- **CLA:** if a second maintainer or outside contributors arrive, a lightweight CLA or DCO keeps the licensing story clean enough to later carve an open-core boundary without chasing signatures. Not urgent while solo-maintained; worth deciding before the contributor base grows.

## 7. Sequencing and open questions

Sequencing:

1. **Now:** ship the website (`spec/21`) with a waitlist. Measure demand before building any control plane. The free OSS product is the top of the funnel.
2. **On real demand:** build the minimal control plane + credential broker (`P7-PROD-04`) — the prerequisite for *any* billing, and also the right foundation for teams (`P4-SYNC-08` multi-workspace) and server-derived metrics.
3. **Then:** managed Free + Individual tiers on operated lifecycle enforcement (`P7-PROD-02`) and a version-skew policy (`P7-PROD-03`).
4. **Later, sales-led:** Team/Enterprise once an org concept and audit export exist (`P7-PROD-06`).

Open questions:

- **Free-tier storage cap.** What ciphertext volume is a real solo workspace? Needs measurement from dogfood data before a number is set.
- **Per-workspace vs per-seat at the Team tier.** Per-workspace is meterable today; per-seat needs the account↔human mapping. Start per-workspace, revisit if teams ask for seat-based procurement.
- **Where the control plane runs.** `spec/19` sketches Fly.io + Neon; that is provisional and gated on this document's demand signal.
- **Managed-tier COGS.** Real gross margin can't be computed until the control plane exists and storage-per-workspace is measured; the §5 prices are a hypothesis to validate, not a costed model.
- **Name.** The collision noise (`spec/21` §6) may warrant a distinguishing modifier before heavy spend — decide alongside the domain purchase.

## 8. References

All accessed 2026-07-10. Comparable-product pricing pages: Tailscale (`tailscale.com/pricing`, `/blog/pricing-v4`, `/opensource`); Ngrok (`ngrok.com/pricing`, `/docs/pricing-limits`); Docker (`docker.com/pricing`, `/blog/revisiting-docker-hub-policies-prioritizing-developer-experience`); Doppler (`doppler.com/pricing`); Infisical (`infisical.com/pricing`, `/blog/secrets-manager-pricing`, `/docs/self-hosting/ee`); 1Password (`1password.com/pricing`); Raycast (`raycast.com/pricing`); GitHub Codespaces (`github.com/pricing`, `docs.github.com` billing); Gitpod/Ona (`ona.com/pricing`, rebrand/sunset stories); Coder (`coder.com/pricing`). Infrastructure: Cloudflare R2 pricing (`developers.cloudflare.com/r2/pricing`), Workers limits (`developers.cloudflare.com/workers/platform/limits`). Pricing-model precedents: Terraform/BUSL → OpenTofu, Docker Hub 2025 reversal (`theregister.com`), JetBrains 2025 hike, Gitpod→Ona pivot (`theregister.com`). Internal: `spec/19_CLOUD_PROVISIONING_GUIDE.md` (hosting), `spec/21_WEBSITE_PLAN.md` (waitlist), the 2026-07-10 Pass-7 audit `P7-PROD-*` and `P7-HUB` findings (engineering prerequisites + cost profile).
