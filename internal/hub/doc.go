// Package hub implements the Cloudflare R2 zero-knowledge production backend
// for the DevStrap sync hub.
//
// The logical DevStrap Hub (HUB-08) has three explicit implementation backends,
// each behind the one sync.Hub interface defined in internal/sync:
//
//   - file: the file-backed test backend (internal/sync.FileHub), retained ONLY
//     for tests and the --hub-file spike.
//   - s3/r2: the direct Cloudflare R2 backend (R2Hub in this package), using S3
//     credentials and cursor-based polling with backoff. This is the production
//     backend for the single-user fleet.
//   - http/sse: a future relay backend (deferred) that adds live push and
//     multi-tenant routing. mTLS and SSE belong only to this later relay, not
//     to the R2-direct path.
//
// R2-direct uses S3 credentials and cursor polling (ListObjectsV2 with
// start-after) with backoff. It does not require a bespoke server — R2 is a
// managed object store. The HTTP/SSE relay is revisited only if a transport
// need (live push, multi-tenant routing) outgrows R2-direct.
package hub
