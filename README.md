# AdobeConnect-file-recorder

Downloads and reconstructs Adobe Connect **Standard AV** (FLV/VP6-era) offline
recordings — the old recordings that have **no download button and no MP4
rendition** — into a single MP4 with embedded audio, plus sidecar files.

The default output is a **1280×720 video approximating the presented layout**:
the shared content on the left, with the attendee list and a rolling chat
history (time-synced, stays visible in a bounded box) burned into a 320px
sidebar on the right. The sidebar box spans the height of the recorded video
itself (letterboxed content), so its border sits exactly on the video's
top/bottom edge.

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

# Same, logging in with credentials from a .env file (no --session needed)
bin/ac-rec download 'https://webinar5.um.ac.ir/p15rdeovw4ge/'

# Offline: reconstruct from an already-downloaded ZIP
bin/ac-rec reconstruct ~/Downloads/p15rdeovw4ge.zip
```

Get the session cookie: log in with your browser → DevTools → Network →
any request → Request Headers → `Cookie: BREEZESESSION=…`. Pass it via
`--session`, or add arbitrary cookies with `--cookie Name=Value` (repeatable).
Alternatively put credentials in a `.env` file in the current directory
(copy [`.env.example`](.env.example)):

```sh
AC_USERNAME=you@example.com
AC_PASSWORD=secret
```

(`USERNAME=`/`PASSWORD=` work too; the file is gitignored.) When `--session`
is absent and `.env` exists, the download logs in via `/api/xml?action=login`
and uses the returned session cookies automatically.

Output lands in `./recordings/<recording-title>/` (`download` command; `serve`
groups queued recordings by the folder they belong to in Adobe Connect —
`./recordings/<folder-id>_<folder-name>/<recording-title>/`, with a `folder.json`
beside them mapping the machine id to the human name; unresolvable folders land
in `_unfiled/`):

| File | Content |
|---|---|
| `recording.mp4` | 1280×720 H.264 + AAC layout video (content + burned-in chat/attendees sidebar) |
| `recording.mp3` | audio only (with `--mp3`) |
| `chat_log.txt` | chat with `HH:MM:SS` recording-relative timestamps |
| `attendees.txt` | attendee names (deduplicated) |
| `metadata.json` | duration, segment counts, warnings |

Flags: `--raw` restores the old full-frame output at source resolution;
`--mp3` adds the MP3 export; `--resolution 480p|720p|1080p` lowers/raises the
output resolution (default 720p); `--fps` (default 25), `--preset` and `--crf`
speed up encoding — e.g. `--fps 12 --preset ultrafast` renders ~4x faster for
screen-share recordings; `--name`/`--out`/`--overwrite`/`--session`/
`--cookie` as shown by `ac-rec help`.

## How it works

1. Derive `…/<id>/output/<id>.zip?download=zip` from the recording URL.
2. Download with session cookie (retry, ZIP-magic validation, no login-HTML passthrough).
3. Parse `mainstream.xml` → per-stream segments (`streamRemoved`, `__stop__`).
4. One FFmpeg pass: 720p canvas + content overlay + burned-in ASS subtitles
   (attendee list, chat history); VP6/Nellymoser → H.264/AAC. Video gaps → black
   video; audio segments are placed at their timeline offsets (`adelay`+`amix`)
   and padded to the exact timeline length, so output duration matches the
   recording with no drift (overlapping publishers are mixed, not concatenated).
   `--raw` skips the layout and keeps source resolution.
5. Validate output duration against the timeline; report honestly.

Skipped for now: browser login automation (use `--session` or `.env` credentials),
exact pod-position
layout events, camera PiP, Approach A fast path for Enhanced-AV recordings.
Add when: a recording actually needs it.

## Queue service

`ac-rec serve` runs N workers draining a persistent queue, plus a status
page and a tiny JSON API:

```sh
cp .env.example .env   # set AC_USERNAME/AC_PASSWORD, PORT, MAX_WORKERS, …
bin/ac-rec serve [--env .env]

curl -X POST localhost:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://webinar5.um.ac.ir/p15rdeovw4ge/"}'
```

Open `http://localhost:8080/` for the status page; `GET /api/tasks` and
`GET /api/tasks/{id}` show queue state. `.env` keys: `PORT`, `DATA_DIR`,
`OUT_DIR`, `MAX_WORKERS`, `AC_USERNAME`/`AC_PASSWORD`, `DEFAULT_RESOLUTION`,
`DEFAULT_FPS`, `DEFAULT_PRESET`, `DEFAULT_CRF`, `EXPORT_MP3`, `RETRY_COUNT`
(env vars override the file). Docker:

```sh
docker compose up --build
```
