---
last_reviewed: 2026-07-10
tracks_code: [README.md]
---
# DevStrap — Website Plan


DevStrap has a released CLI (`v0.1.1`), a Homebrew cask, a curl installer, and a verifiable supply chain — but no web presence. Someone who hears "DevStrap" today lands on a GitHub README or a parked domain. This document specifies the marketing + docs site: what it must do, how it is built, where it is hosted, and how it ships in phases without becoming a second product to maintain.

The site serves two audiences at once and must not blur them: the open-source developer who wants to `brew install` and read docs, and the prospective customer of the planned managed-hub tier (`spec/20`) who wants to join a waitlist. Everything below keeps those two conversion paths visible and distinct.

## 1. Goals & audience

**Primary goal — OSS adoption.** Turn a curious developer into an installed CLI in one screen. The hero must communicate the product in one sentence, show it working, and put `brew install Reederey87/devstrap/devstrap` (and the curl one-liner) above the fold. Success is measured by installs and docs engagement, not time-on-site.

**Secondary goal — hosted-tier waitlist.** DevStrap's managed-hub tier (free-with-limits + subscription, planned in `spec/20`) needs demand signal before it ships. The site captures emails for a waitlist with a clear, honest pitch: *the CLI is free and self-hostable forever; the hosted tier is a managed hub you don't have to run.* Never imply the OSS product is crippled without it.

Two audiences, two calls to action, never competing on the same square inch:

- **OSS developer** → install commands, docs, GitHub, security story. Lands from HN/Reddit/GitHub/word-of-mouth.
- **Hosted-tier prospect** → waitlist form, a one-line "what you get" pitch, a pricing placeholder. Lands from the same channels but converts differently.

**Success metrics.** (a) Install-command copy events and `docs/` pageviews as the OSS funnel; (b) waitlist sign-ups as the commercial funnel; (c) time-to-first-meaningful-paint under 1.5 s and a Lighthouse performance score ≥ 95 (a developer tool's site that is slow undercuts the pitch); (d) privacy-respecting analytics coverage. No vanity metrics — bounce rate on a single-page install site is meaningless. Note the dependency (§7): the two *funnel* metrics (a, b) are **event** metrics, and Cloudflare Web Analytics is pageview-only — so measuring install-copies and waitlist submits requires either an event-capable analytics layer (Plausible) or reading the counts directly from their source (the waitlist store's own row count; a copy-button handler posting to a Pages Function). Treat (a)/(b) as gated on that choice, not as free from a pageview tracker.

## 2. Information architecture

Launch is deliberately small. The pages below are the full eventual map; §8 phases which of them ship when.

- **Home (`/`)** — one-sentence value prop ("Your code. Your structure. Always in sync."), the animated terminal hero (§4), the split CTA (install vs. waitlist), a three-tile "how it works" (Git owns content · signed namespace map · age-encrypted secrets), and a compact feature grid lifted from the README. Ends with the security one-liner and links out.
- **Install (`/install`)** — the two happy paths (brew, curl), then release-binary / `go install` / build-from-source, and the cosign/SLSA verification block. This is a near-verbatim render of the in-repo `docs/install.md`; do not fork the content (see below).
- **Docs (`/docs/...`)** — quickstart, self-hosting, command reference. **Single-source strategy: the site renders the in-repo `docs/` tier at build time, it never re-authors it.** DevStrap already maintains `docs/install.md`, `docs/quickstart.md`, and `docs/self-hosting.md` as the user-facing tier distinct from the `spec/` design corpus. The website's Docs section pulls those Markdown files from the DevStrap repo during the build (git submodule or a pinned build-time fetch against a tag) and renders them through Starlight. This keeps one source of truth: a docs fix lands in the product repo, where the spec-drift gate already governs it, and the site picks it up on its next build. The alternative — copying docs into the site repo — guarantees drift and is rejected.
- **Security (`/security`)** — the differentiator. Most dev-tool sites hand-wave security; DevStrap has a real threat model (`spec/15`), end-to-end encryption by construction, and a verifiable supply chain. This page tells that story concretely: zero-knowledge hub (repo bytes never traverse it; env/draft blobs are age-encrypted client-side; the event log is envelope-encrypted under a per-epoch Workspace Content Key), device-trust with out-of-band fingerprint pairing, and the cosign-keyless + SLSA + SBOM release verification a visitor can run themselves. Links to `SECURITY.md` for disclosure. This page earns trust that a feature grid cannot.
- **Pricing (`/pricing`)** — a placeholder until `spec/20` executes: "The CLI and self-hosted hub are free and open-source. A managed hub tier is coming — join the waitlist." No numbers until the commercial plan commits to them; a wrong price on the site is worse than no price.
- **Changelog / Blog (`/blog`, `/changelog`)** — changelog is generated from GitHub Releases (already the source of truth via GoReleaser); blog is optional long-form for launch posts and security write-ups. Ships last.
- **GitHub** — an external link in the nav, not a page. The repo is the credibility anchor; make it one click from everywhere.

## 3. Tech stack recommendation

**Recommendation: Astro + Starlight + Tailwind, deployed on Cloudflare Pages.**

The site is static content plus one waitlist form; there is no application backend at launch. That shape rewards a static-site generator with a first-class docs theme, and it argues against a full React app framework. Three reasons decide it:

1. **Starlight is purpose-built for exactly the Docs + Security sections** — sidebar nav, search, code blocks, and Markdown-first authoring — so the single-source docs strategy (§2) is native, not bolted on. Astro ships zero JavaScript by default, which is the right performance posture for a developer tool that sells on being lean.
2. **The product already lives on Cloudflare (R2).** Keeping the site on Cloudflare Pages consolidates the vendor surface: one dashboard, one billing relationship, and Cloudflare Web Analytics (free, cookieless — §7) with no extra integration. The waitlist form's server side is a Cloudflare Pages Function writing to KV or D1, or a POST to a form service — no separate backend deploy.
3. **Cost and honesty.** The user's default stack (Next.js/Vercel/Convex/Clerk) is excellent for an app with auth and a database. This site has neither at launch. Pulling in that stack now buys complexity the launch doesn't need and splits infrastructure across two clouds.

**Alternatives considered:**

| Stack | Why it loses (for launch) |
|---|---|
| **Next.js + Vercel** (the stack default) | Right tool once the hosted tier ships a dashboard with auth; overkill for static content + one form, and splits infra off Cloudflare. Revisit for the dashboard, not the marketing site. |
| **Astro on Vercel** | Fine, but forfeits the Cloudflare consolidation and the free cookieless analytics; no upside over Pages here. |
| **Plain Astro (no Starlight)** | Loses the docs theme; we'd hand-build sidebar/search that Starlight gives free. |
| **VitePress / Docusaurus** | Docs-first but weaker as a marketing landing page; Starlight + Astro pages covers both in one build. |

**Where the stack default re-enters.** If/when the hosted tier ships a customer dashboard (account, hub provisioning, billing), that is a separate application and the right place for **Convex** (data + realtime) and **Clerk** (auth) on **Vercel** or Cloudflare Workers. It lives at `app.devstrap.dev` or `dashboard.devstrap.dev`, deployed independently from this marketing/docs site. Note it in `spec/20`; do not pre-build it here.

## 4. Design direction

**Terminal-first, dark-by-default.** DevStrap is a CLI for developers; the site should feel like the tool. Dark background as the primary theme (with a clean light mode — Starlight supports both and respects `prefers-color-scheme`), monospace for code and command accents, generous whitespace, no stock photography, no gradients-for-their-own-sake.

- **Hero concept: an animated terminal demo.** The single most persuasive asset is the product's own core loop shown running. Record `devstrap sync` reconstructing a `~/Code` tree on a second machine — the eager-clone payoff — as a terminal cast. Use **VHS** (charmbracelet/vhs) to script a deterministic, re-recordable cast rendered to an optimized GIF/WebM, or **asciinema** for a selectable-text player. VHS is preferred: the output is a scripted `.tape` checked into the repo, so the demo is reproducible and versioned rather than a one-take screen recording. The hero loops it, muted and auto-playing, with the install command directly beneath.
- **Typography.** A clean grotesk/sans for prose (system stack or a self-hosted variable font — no external font CDN if it costs a render-blocking request), a monospace (JetBrains Mono, Berkeley Mono, or the system mono) for commands, paths, and the terminal aesthetic.
- **Color.** Near-black base, one signal accent used sparingly for CTAs and terminal-success cues (a terminal green or a single brand hue — align with the existing `icon.png`/`repo_image2.png` palette so the site and repo read as one brand). Status and diagrams must not rely on color alone (WCAG 2.2 AA, per the product's own web guardrails in `spec/02`).
- **Social cards & favicon.** OG/Twitter cards per page (a dark card with the wordmark + one-line pitch; per-page titles for docs). Favicon and logo derive from the existing `icon.png`; commission or refine a simple wordmark ("DevStrap" one word) and a mark that survives at 16px. Keep the passport/namespace metaphor subtle — this is a CLI, not a travel app.

## 5. Hosting & deployment

**Provider: Cloudflare Pages** (see §3 for why — vendor consolidation with R2, free cookieless analytics, global edge, generous free tier). Git-connected builds with automatic preview deploys per PR and production on merge to the site repo's `main`.

**Separate repo, not a `/website` directory in the monorepo — recommended.** The DevStrap monorepo enforces a strict spec-drift + work-log gate on every PR (`cmd/spec-drift`, `TestEveryCommandIsDocumented`, mandatory `spec/18` work-log entries). That gate is correct for the product but would tax every copy tweak and CSS change on the site — a one-line hero edit should not require a work-log entry and a spec mapping. A separate `devstrap-web` (or `devstrap-site`) repo lets the site iterate at marketing speed with its own lightweight CI (build + Lighthouse budget check + link check).

The trade-off is real and worth stating: single-sourcing docs (§2) now crosses a repo boundary. Mitigate with a git submodule pinning the product repo's `docs/` at a tag, or a build-time fetch of those files pinned to a release; a scheduled rebuild (or a repository-dispatch webhook from the product repo on docs changes) keeps the published docs current. This is a small amount of build plumbing in exchange for not dragging the spec-drift gate across every site PR — a good trade.

**CI/CD.** Site repo → Cloudflare Pages via GitHub integration. Every PR gets a preview URL; merges to `main` promote to production. Add a CI step that fails the build on a Lighthouse performance regression and on broken internal/external links, so the "fast and correct" promise is enforced, not aspirational.

## 6. Domain & naming

**Recommendation: register `devstrap.dev` as the primary domain.**

- `.dev` is on the HSTS preload list, so it **forces HTTPS** at the browser level — a fitting default for a security-forward tool — is developer-native (Google-operated registry), and costs ~$12–15/yr. It sidesteps the existing collisions: `devstrap.net` is a WordPress theme product, `devstrap.xyz` is a tools site, and the `.com` is parked.
- **On `devstrap.com`:** parked at $4,150 outright (or $169/mo lease). Not worth it at the current stage. The product is alpha, revenue is a plan not a fact, and `.dev` reads as more credible to the developer audience than a generic `.com`. Revisit the `.com` purchase only if the hosted tier gains real traction and the type-in/`.com` confusion becomes a measurable acquisition leak — at that point $4,150 is a rounding error; today it is most of a year's runway on nothing. Do not lease ($169/mo builds no equity).
- **Defensive registrations.** If cheap and available, grab `devstrap.io` and/or `devstrap.sh` and 301-redirect them to `.dev` to deny squatters and cover type-ins. `getdevstrap.com` is a fallback only if `.dev` is somehow unavailable. **All of these — `.dev`, `.io`, `.sh`, `.app`, `getdevstrap.com` — are unverified: WHOIS-check each before assuming availability or purchasing** (research flagged them as "no DNS, likely available," which is not the same as registrable). Buy `.dev` first; add defensive TLDs in the same session.
- **Name-collision reality and SEO.** "DevStrap" is not unique — at least five small OSS repos, a WordPress theme (`devstrap.net`), a tools site (`devstrap.xyz`), and a taken `@DevsTrap` X handle exist, though no USPTO registration was found. Mitigation: (a) consistent one-word "DevStrap" everywhere, always paired with the "Workspace Passport" concept phrase so search intent disambiguates; (b) keep the GitHub org/repo canonical (`Reederey87/DevStrap`) and, if a second maintainer is recruited (per `spec/02` bus-factor note), consider a neutral GitHub org (`devstrap`) to look less like a personal project — verify the org name is free first; (c) own the branded queries ("devstrap cli", "devstrap sync", "workspace passport") through the Home and Docs meta rather than fighting for the bare word "devstrap"; (d) claim `@devstrap` handles where available and link them from the site footer. Trademark search is out of scope for launch but worth a lightweight USPTO/EUIPO check before any paid marketing spend.

## 7. Analytics & privacy

**No cookies, no consent banner, no third-party trackers.** A developer-tool site that sells on zero-knowledge encryption cannot ship an ad-tech tracker; the medium is the message.

- **Recommendation: Cloudflare Web Analytics** — free, cookieless, no client-side state, and already in the stack since the site is on Cloudflare Pages. Setting no cookies and doing no cross-site tracking **greatly reduces** the consent burden, but "cookieless" does not automatically eliminate every GDPR/ePrivacy obligation (lawful basis for any IP/analytics processing is jurisdiction- and configuration-dependent). Treat "no consent banner" as the *target*, confirmed against the launch jurisdictions and Cloudflare's current data-processing terms before launch, with an opt-out/DNT-respecting fallback ready if a market requires it. This is the zero-friction default.
- **Alternative: Plausible** (self-hostable or cloud) if richer event tracking is needed — e.g. distinguishing install-command copies from waitlist submissions as first-class goals. Also cookieless and consent-banner-free. Adds a small script and a cost; adopt only if Cloudflare's event model proves too coarse for the two-funnel measurement in §1.
- **Waitlist data.** Treat emails as PII: store minimally (email + timestamp + source), never share, disclose usage plainly on the form ("we'll email you once about the hosted tier"), and keep the store (Cloudflare KV/D1 or the form provider) access-controlled. The privacy stance is a feature — state it on the site, don't bury it. Because the form is public, v1 must also define, before exposing it: a **retention duration** (e.g. purge on hosted-tier launch or after N months, whichever first) and an authenticated **deletion** path (not just "honor requests"); **email validation** (format + optional confirmed-opt-in double-opt-in) and **duplicate/replay handling** (idempotent on email, ignore resubmits); and **rate limiting + bot protection** (Cloudflare Turnstile or the Pages Function rate-limiting KV) so the endpoint can't be flooded or used as a spam relay. These are launch-blocking for the form, not nice-to-haves.

## 8. SEO & launch

**On every page:** descriptive `<title>` and meta description, canonical URL, OpenGraph + Twitter card tags, and a generated `sitemap.xml` (via `@astrojs/sitemap`). `robots.txt` is **not** auto-emitted by Astro/Starlight — add it explicitly as a `public/robots.txt` static file (or a route) that points at the sitemap, and cover both in the CI link/asset check so neither silently 404s. Structured data (`SoftwareApplication`) on Home so the CLI surfaces well. Docs pages get per-page titles keyed to commands ("devstrap sync", "self-hosting a hub") to win the long-tail queries developers actually type.

**Key landing queries to own:** "devstrap", "devstrap cli", "workspace passport", "sync ~/Code across machines", "portable dev environment git", "zero-knowledge code sync", "agent worktree fresh branch". The Security page is a differentiated SEO surface — few competitors publish a real threat model — so lean into "zero-knowledge developer sync" and "age-encrypted env sync" terms there.

**Phased launch checklist:**

- **v1 — landing + install + waitlist (ship first).** Single-page Home with the animated hero, install commands, the split CTA, and a working waitlist form. `/install` rendered from `docs/install.md`. Cloudflare Web Analytics live. Domain + HTTPS + OG cards. This is enough to post to HN/Reddit and start collecting waitlist demand.
- **v2 — docs + security.** Wire up the single-sourced Docs section (quickstart, self-hosting, command reference) and the Security page. This converts the traffic v1 attracts into installs and trust. Add sitemap + per-page SEO.
- **v3 — pricing + blog.** Publish `/pricing` once `spec/20` commits to a model, and stand up the changelog (from GitHub Releases) and optional blog for launch posts and security write-ups. If the hosted tier ships a dashboard, that is a separate app (§3), not part of this site.

## 9. Open questions

- **Waitlist backend.** Cloudflare Pages Function + KV/D1 (keeps everything in-stack) versus a hosted form service (faster to stand up, one less thing to run)? Lean in-stack for data ownership, but the form service is an acceptable v1 shortcut.
- **Docs single-sourcing mechanism.** Git submodule pinned to a tag, versus a build-time fetch, versus a repository-dispatch webhook that triggers a site rebuild on `docs/` changes? Pick based on how fresh docs must be — a nightly rebuild is likely enough; instant is nice-to-have.
- **`.com` decision timing.** Set an explicit trigger for revisiting the $4,150 `.com` purchase (e.g. hosted-tier waitlist crosses N sign-ups, or measurable type-in traffic loss) rather than deciding emotionally later.
- **Blog scope.** Is the blog worth maintaining, or is a changelog (auto-generated from releases) plus occasional GitHub Discussions enough? Defer until v3; don't commit to a content treadmill prematurely.
- **Relationship to `spec/20`.** The pricing page and waitlist copy depend on the commercial plan's shape (free-tier limits, subscription price points, what "managed hub" concretely includes). This site plan assumes `spec/20` lands those decisions; the site should not invent them.
- **Second maintainer / org rename.** If bus-factor recruitment (`spec/02`) moves the repo to a `devstrap` GitHub org, the site's GitHub links, canonical naming, and install tap (`Reederey87/devstrap`) all shift — sequence the domain, org, and tap naming decisions together to avoid churning published URLs.
