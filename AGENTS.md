# Agent instructions

## Project

`ac-rec` downloads and reconstructs Adobe Connect offline recordings. Pure Go
(stdlib only — do not add third-party dependencies). Heavy lifting (decode,
x264 encode, subtitle burn, audio mix) is done by `ffmpeg`, which this code
spawns; the Go side is orchestration (XML parsing, timeline math, argv
building). See README.md for user-facing docs and flags.

## Versioning standard

The single source of truth for the version is the `VERSION` file (plain text).
It holds either a final release `MAJOR.MINOR.PATCH` (e.g. `v0.1.0`) or a
pre-release `MAJOR.MINOR.PATCH-rc.N` (e.g. `v0.2.0-rc.1`). The binary reads
it via `internal/version.Version`, stamped at build time with:

```
go build -ldflags "-X github.com/iman-langaran/AdobeConnect-file-recorder/internal/version.Version=$(cat VERSION)" -o bin/ac-rec ./cmd/ac-rec
```

A plain `go build` without ldflags reports `dev` — that is expected for
dev builds; never hardcode a release number in source.

### Rules the agent MUST follow

1. **Never edit `internal/version/version.go` to change the version.** Bump
   only the `VERSION` file. The `Version` var stays a `"dev"` fallback and is
   set exclusively via `-ldflags -X` at release time.
2. **`VERSION` is the only place a release number is written.** Do not
   duplicate it in README, code, or elsewhere.
3. **Pre-releases** (when the user wants to ship a test build):
   - set `VERSION` to `vX.Y.Z-rc.N` (start at `-rc.1`),
   - `make build`,
   - commit the bump (message like `release vX.Y.Z-rc.N`),
   - tag `vX.Y.Z-rc.N` (annotated),
   - `git push --follow-tags`.
   - Ship further test builds by incrementing `N` (`-rc.2`, `-rc.3`, …).
   - To finalize, promote the pre-release: set `VERSION` to `vX.Y.Z`
     (drop the `-rc.N`), `make build`, commit as `release vX.Y.Z`, tag
     `vX.Y.Z`, push. The final `vX.Y.Z` MUST be the same code state you
     validated as the last `-rc` (or a superset with only fixes).
4. **SemVer**: bump MAJOR for breaking CLI/output-format changes, MINOR for
   backwards-compatible features, PATCH for fixes. `-rc.N` is a pre-release
   of the version it names: `v0.2.0-rc.1` is a test build on the way to
   `v0.2.0`, not a fix on `v0.1.x`.
5. **Ordering**: pre-releases sort BEFORE their final (SemVer rule:
   `v0.2.0-rc.1` < `v0.2.0`). So a `-rc` can be cut at any point during
   feature work, then finalized once stable — no need to finish everything
   before tagging an rc.
6. **Do not create tags or bump `VERSION` on your own initiative.** Feature
   commits do not touch version files. Only cut a release or pre-release when
   the user asks.

## Code standards

- Go stdlib only; no new module dependencies.
- Keep `gofmt`, `go vet ./...`, and `go test ./...` green before committing.
- Trivial/one-line changes need no test; non-trivial logic leaves one runnable
  check behind. Do not add test frameworks.
- Validate real output at `--resolution 480p` (fastest) unless the user wants
  otherwise.
