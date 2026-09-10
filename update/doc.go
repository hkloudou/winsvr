// Package update keeps a local payload binary in sync with a remote copy, using
// cheap HTTP HEAD requests to decide whether a download is needed.
//
// Two validator strategies are provided and can be combined:
//
//   - HTTPValidator inspects standard response headers (ETag, then
//     Last-Modified, then Content-Length) from a HEAD request. It works against
//     any static file host — nginx, S3, a CDN — with no server changes.
//
//   - SidecarValidator fetches a small JSON manifest published next to the
//     binary (e.g. helper.bin.json) carrying an explicit {version, size, crc64}.
//     It enables semantic versioning and strong end-to-end integrity checks,
//     at the cost of publishing that file.
//
// The package is pure Go and builds on every platform, so update logic can be
// unit-tested off Windows.
package update
