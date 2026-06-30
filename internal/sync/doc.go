// Package sync is a Phase-0 experimental SPIKE, not the production hub protocol.
//
// It proves the local-first spine the rest of the product will build on:
//   - Hybrid Logical Clock ordering with a device-id tiebreaker (hlc.go),
//   - an append-only, content-hashed, idempotent event log (events.go),
//   - deterministic replay and order-independent same-path/different-remote
//     conflict DETECTION across two local roots,
//   - HLC-gated tombstones, rename, and a clock-skew quarantine guard.
//
// Scope and assumptions:
//   - Namespace state is treated as single-writer-per-path most of the time;
//     the path/remote conflict class is surfaced for the user and never
//     auto-merged. The safe-automatic cases defined in spec/03 (duplicate
//     skeleton creation, heartbeat latest-wins, recreate-missing-skeleton) may
//     still be resolved without prompting.
//   - The on-wire hub protocol is NOT defined here. The device_sig and
//     prev_event_hash chain columns are written and validated locally as a
//     deliberate, accepted divergence from the original "defer until the hub"
//     plan (see docs/audits/AUDIT_RECOMMENDATIONS.md ARCH-2 / spec/07); the chain FORMAT
//     should still be re-reviewed before a production hub freezes it.
//   - FileHub (hub.go) is a file-backed TEST hub only.
//
// Before building a bespoke devstraphub, re-evaluate whether a hidden manifest
// git repo (spec/01, spec/04) is a faster transport than a new service.
package sync
