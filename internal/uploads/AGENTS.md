# internal/uploads

The source tarball's whole life: accepting one onto the volume, recording what
landed, spending the single use token a build presents to fetch it back, and
sweeping the expired ones. Two files: `uploads.go`, and `pack.go` for source a
caller carried inline instead of uploading.

It imports neither the store, `net/http`, nor `client-go`. It declares the narrow
`Store` interface it needs, which [internal/store](../store) satisfies through an
adapter in that package, and it takes an `io.Reader` rather than a request.

Governing specs: [0004](../../docs/specs/0004-build-pipeline/index.md) for the
fetch token and the tarball's lifetime, and
[0022](../../docs/specs/0022-publishing-the-deploy-path/index.md) for the
ceiling, the unclaimed cap and the sweep.

## Conventions

- **Nothing a caller sent decides where a file goes.** The path is the upload
  directory joined with an id this platform generated, so no request can steer a
  write. Keep it that way: a caller supplied name reaching `filepath.Join` here
  is a path traversal.
- **`Pack` bounds nothing, deliberately.** It composes a tarball in memory from a
  map of path to content and hands the reader to `Accept`, which applies every
  bound there is. A size check inside `Pack` would be a second place for the
  ceiling to live and so a second place for it to drift; the transport bounds the
  request body long before the ceiling does. What `Pack` does own is the path
  rule: absolute, empty, NUL bearing and climbing names are `ErrBadPath` before a
  byte is written. That refusal is also made at extraction, and making it twice is
  the point, because the extractor's version ends a deployment the platform has
  already spent a build pod on (spec 0026, AC-3).
- **The body stops being read one byte past the cap.** An oversized or endless
  body therefore costs one byte more than the limit rather than a volume. That is
  the second of the two size gates: the first is the declared `Content-Length`,
  refused in [internal/httpapi](../httpapi) before a byte is read, and this one
  catches a body that declared nothing or lied (spec 0022, AC-12).
- **The gzip check is a cheap refusal, not validation.** It reads the first two
  bytes back and refuses a body that was plainly never a gzip stream. The archive
  itself is walked later, by the reconcile loop, and a body that passes here is
  not yet known to be a usable tarball.
- **An upload is immutable once its hash is recorded**, and that hash is what the
  build pod checks its download against. `Open` opens read only and never
  rewrites.
- **Single use is the store's conditional update, not a read then a write.** Two
  builds presenting the same fetch token cannot both win, and making that a read
  followed by a write is how they would.
- **Minting a token again resets the redemption**, so a resumed or retried build
  gets a working token rather than a spent one. The raw value goes straight onto
  the build Job's init container and is never persisted or logged; only its hash
  is stored.
- **The sweep deletes the file before the row, and the order is the safe one.** A
  row whose file is already gone is a case the fetch path handles and answers as
  expired. A file whose row is gone is a leak nothing would ever find again.
- **The sweep excludes referenced uploads in the query rather than discovering
  them by error.** A row a deployment still names carries the source a release
  was built from, and deleting it is what `ON DELETE RESTRICT` is there to refuse
  (spec 0022, AC-18).
- **`Window` is one hour and is a constant here**, not configuration. An agent
  uploads and deploys in one breath, so anything longer is only a wider window
  for a leaked id.
- **A failed discard is logged loudly rather than dropped.** It leaves a stray
  file the retention sweep does not know about, which is worth noticing.

## Tests

`uploads_test.go`, against a real temp directory and a real SQLite file, no store
mocking. `pack_test.go` is pure and needs neither. The cases worth keeping intact are the two that assert what is *not*
left behind: `TestAcceptRefusesABodyOverTheCapAndLeavesNothingBehind` and
`TestAcceptDiscardsTheFileWhenTheStoreRefusesTheRow`. A refusal that still costs
the platform a file is the failure mode this package exists to avoid, and it
passes every test that only checks the status code.

The route level half of the same behaviour, including the unclaimed cap and the
sweep end to end, is in `internal/httpapi/deployhost_test.go`.

_Drafted by /sync at the engineer's request, worth a quick human pass._
