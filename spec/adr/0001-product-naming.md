---
status: accepted
date: 2026-06-26
---

# 0001 — Product Naming

## Context

Early specs used three working names interchangeably — "DevStrap", "Workspace
Passport", and "StrapFS" — and only *recommended* a mapping without making it
binding. `spec/00` even titled itself "DevStrap / Workspace Passport", which
reads as if the two are synonyms. For an agent-consumed spec corpus, two
competing brand strings with no single source of truth cause inconsistent usage.

## Decision

- **DevStrap** is the product. The Go module, binary, and all code and
  user-facing strings use only "DevStrap".
- **Workspace Passport** is the *core concept* — the portable, managed code
  namespace that appears identically on every device. It is a tagline for the
  idea, never a product or binary name.
- **StrapFS** is the *future* optional virtual filesystem layer (Phase 4),
  reserved for when/if a File Provider / FUSE layer is built.

## Consequences

- Other docs reference this ADR instead of re-deriving the naming.
- "Workspace Passport" must not appear as a product/binary string outside this
  ADR and the concept tagline in `spec/00`/`spec/02`.
- The non-recursive spec-drift gate (`spec/*.md`) does not scan `spec/adr/`, so
  this file uses MADR frontmatter (status/date) rather than the
  `last_reviewed`/`tracks_code` frontmatter the gate requires.
