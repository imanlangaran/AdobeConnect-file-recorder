# AdobeConnect-file-recorder

Downloads and reconstructs Adobe Connect **Standard AV** (FLV/VP6-era) offline
recordings — the old recordings that have **no download button and no MP4
rendition** — into a single MP4 with embedded audio, plus sidecar files.

Plan & research: [`docs/ADOBE_CONNECT_OFFLINE_RECORDING_PLAN.md`](docs/ADOBE_CONNECT_OFFLINE_RECORDING_PLAN.md)

## Requirements

- Go ≥ 1.22 (build only)
- `ffmpeg` + `ffprobe` on PATH

## Build

```sh
go build -o bin/ac-rec ./cmd/ac-rec
go test ./...
```

## Usage

```sh
# Full pipeline: download archive + reconstruct
bin/ac-rec download 'https://webinar5.um.ac.ir/p15rdeovw4ge/' --session '<BREEZESESSION>'

# Offline: reconstruct from an already-downloaded ZIP
bin/ac-rec reconstruct ~/Downloads/p15rdeovw4ge.zip
```

Get the session cookie: log in with your browser → DevTools → Network →
any request → Request Headers → `Cookie: BREEZESESSION=…`. Pass it via
`--session`, or add arbitrary cookies with `--cookie Name=Value` (repeatable).

Output lands in `./recordings/<recording-title>/`:

| File | Content |
|---|---|
| `recording.mp4` | H.264 + AAC, timeline-synced (gaps filled with black/silence) |
| `chat_log.txt` | chat with `HH:MM:SS` recording-relative timestamps |
| `attendees.txt` | attendee names (deduplicated) |
| `metadata.json` | duration, segment counts, warnings |

## How it works

1. Derive `…/<id>/output/<id>.zip?download=zip` from the recording URL.
2. Download with session cookie (retry, ZIP-magic validation, no login-HTML passthrough).
3. Parse `mainstream.xml` → per-stream segments (`streamRemoved`, `__stop__`).
4. One FFmpeg pass: VP6/Nellymoser → H.264/AAC, timeline gaps → black video / silence.
5. Validate output duration against the timeline; report honestly.

Skipped for now: browser login automation (use `--session`), Approach A fast
path for Enhanced-AV recordings, camera-only output, whiteboard-as-video.
Add when: a recording actually needs it.
