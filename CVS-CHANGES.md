# CVS fork of tuya-ipc-terminal

This is a pinned, patched fork of [seydx/tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal)
(MIT) maintained for the Cloud Video Surveillance & Evidence Retention project.
It bridges Tuya WebRTC cameras (initially a CORN CHM-302) to a local RTSP
endpoint that FFmpeg records from. Branch: `cvs`. First release: `v0.0.6-cvs.1`.

Nothing in this repository is environment-specific: no credentials, cookies,
IP addresses or `.tuya-data` files. Session state is created at runtime in
`./.tuya-data` (ignored by git and docker).

## Base

| | |
|---|---|
| Upstream | `seydx/tuya-ipc-terminal` `main` @ `d65b3e9` (v0.0.6) |
| Base of `cvs` | upstream PR #33 head `e5340a9355d579dae66bf1eccd2beff18c4303f9` (YaannnTech `fix/rtsp-stream-lockup`: RTSP path matching, codec negotiation, RTP delivery, lock fixes) |

PR #33 is the version the project validated with the CORN CHM-302 SD stream
(`rtsp://127.0.0.1:8555/CORN_CHM-302/sd`, H.264 640x720 ~15 fps).

## Cherry-picks (in order)

| Source | Source SHA | New SHA on `cvs` | Outcome |
|---|---|---|---|
| upstream PR #28 (zombie `/sd`: `waiter.Done` for non-HEVC sd) | `6e0386496cc3094f220827185e1462c325446af3` | `09987d6` | Clean. |
| upstream PR #6 (`--address` listen flag) | `31f494c6443ac967f63879dc8c5f39bdda07e305` | `44c86c3` | Clean. Follow-up `fbe240f` (below) adds `--listen` and fixes IPv6. |
| dobriys fork: SPS/PPS `sprop-parameter-sets` in SDP | `cefbd6a28d582476ed6b7b928efe387166d67942` | `2046f56` | Conflict in `pkg/rtsp/protocol.go`: PR #33 changed `generateSDP(camera, baseURL, streamResolution)`, dobriys changed it to `generateSDP(stream, baseURL)`. Kept PR #33's signature (used by its tests) as a wrapper; SPS/PPS logic lives in `generateSDPWithParameterSets(..., sps, pps)`, fed from the stream's RTP forwarder in `handleDescribe`. |
| dobriys fork: device-supported clarity for HD | `e371e78dba68218254c04ea2138082bb0e3dc82c` | `5691880` | Conflict in `pkg/rtsp/bridge.go` with PR #28's restructuring; kept `waiter.Done` for every non-HEVC stream, `pickVideoClarity()` for hd. Dropped the `main.go` version bump to 0.0.7. |
| dobriys fork: clarity for SD too | `5482cc6bb356b6af60f6afbdbd18f0c5d23e9445` | `566b921` | Conflict in `bridge.go`. Applied, but for sd a clarity is only sent when the device advertises `vedioClaritys`: `pickVideoClarity` returns 0 for an empty list and 0 is the legacy hd value, which could switch the validated CORN sd stream. Dropped the README hunk (advertises the dobriys image). |
| Dependabot PRs #4, #17, #18, #22 | `d649a8d`, `4703a0e`, `10e319b`, `a4bd136` | `62e0eb0` | Applied with `go get` + `go mod tidy` instead of literal cherry-picks (they rewrite the same go.mod lines). pion/interceptor v0.1.39 (rtp v1.8.18), golang.org/x/crypto v0.45.0 (x/net v0.47.0, x/term v0.37.0, x/sys v0.38.0), paho.mqtt.golang v1.5.1 (x/sync v0.17.0), pion/dtls/v3 v3.0.11 (pion/logging v0.2.4, + pion/transport/v4 v4.0.1). Newest wins on shared modules. `go` directive 1.23.0 -> 1.24.0 (required by x/crypto 0.45). |

Nothing was skipped: none of the picks turned out redundant with PR #33.

## Own patches

| Commit | Change |
|---|---|
| `fbe240f` | `--listen <host>` (default **127.0.0.1**, loopback only); `--address/-a` from PR #6 kept as a deprecated alias. Address built with `net.JoinHostPort`, so `::`/`[::]` work (PR #6 produced `:::8554`). |
| `5d2a9f7` | Keyed struct literals so `go vet ./...` passes (no behaviour change). |
| `5300e94` | **Waiter race fix** (`pkg/utils/waiter.go`): `Done()` released waiters before storing the error, so `Wait()` could return `nil` for a failed WebRTC/MQTT attempt (data race under `-race`). |
| `b232dfa` | **Media watchdog.** If a stream (starting, connecting or active) receives no video RTP for the timeout (default 20 s, measured from max(startup, last packet)), it logs `watchdog: stream <device>/<res> no media for Ns, tearing down` (WARN, fields `device`, `device_id`, `resolution`, `clients`, `idle`) and tears the stream down **even with RTSP clients attached**: the stream is marked dead and removed from the server map first, then every RTSP client connection is closed (FFmpeg sees EOF and reconnects), then the bridge is stopped (peer connection, MQTT, forwarder). Dead streams are never reused or restarted; a reconnecting client always gets a fresh WebRTC session (`AddClient` on a dead stream fails, `handleConnection` retries once). `WebRTCBridge.Stop` is now idempotent and also releases bridges that are still negotiating or failed half-way (upstream leaked the MQTT client and peer connection of every failed startup). A failed startup also disconnects attached clients. |
| `89358cf` | `--media-timeout` flag; env fallbacks for the CVS flags. |
| `39fc38f` | **Session keeper / automatic re-login** (`pkg/session`). Every check interval (default 6 h) each stored session is validated with an authenticated call; it is renewed by non-interactive email/password login when Tuya rejects it, when it is expired, or when it is within the renew margin (default 24 h) of its expiry (earliest cookie expiry if stored, else `LastRefresh` + 96 h). New sessions are validated before replacing the stored one; a failed login never overwrites it. Failures log at ERROR and the process keeps running: `session: re-login failed`, `session: expiring, no credentials configured`, `session: invalid, no credentials configured`. Failed re-logins are retried after 30 min. `SaveUser` is now atomic (temp file + rename, 0600). The interactive QR / password `auth` commands are unchanged. |
| `13c02bf` | **Health endpoint** (`--health-listen`, default off; `8556` binds `127.0.0.1:8556`): `GET /healthz` returns JSON with `status` (`ok`/`degraded`/`down`, 503 when down), `version`, `rtsp_listen`, `media_timeout_seconds`, `session_keeper_enabled`, `streams[]` (`device`, `device_id`, `rtsp_path`, `resolution`, `state`, `clients`, `started_at`, `last_media_at`, `last_audio_at`, `media_age_seconds`) and `sessions[]` (`user_key`, `region`, `valid`, `checked_at`, `expires_at`, `last_refresh`, `last_relogin_at`, `credentials_configured`, `error`). Times are UTC RFC3339, `null` = never. |
| `51dbb80` | Session log times as RFC3339. |
| `8b1cc52` | Pinned multi-stage `Dockerfile`, `.dockerignore`, `VERSION` = `0.0.6-cvs.1` (now a `var` so `-X main.VERSION=` works). |
| `6442f3e` | CI (`ci.yml`: gofmt, build, vet, `test -race`, amd64+arm64 docker build) and release (`release.yml`: on `v*` tags push `ghcr.io/practicaldev101/tuya-ipc-terminal-cvs:<version>` and print the digest). Actions pinned by SHA. |

## New flags and environment variables (`rtsp start`)

An explicitly passed flag wins over the environment. Durations accept Go
syntax (`20s`, `6h`) or integer seconds.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--listen` | `TUYA_RTSP_LISTEN` | `127.0.0.1` | RTSP bind host. `0.0.0.0` / `::` to expose (not recommended). |
| `--address`, `-a` | | | Deprecated alias of `--listen` (PR #6). |
| `--media-timeout` | `TUYA_MEDIA_TIMEOUT` | `20s` | Media watchdog timeout; `0` disables. |
| `--session-check-interval` | `TUYA_SESSION_CHECK_INTERVAL` | `6h` | Session validation period; `0` disables the keeper. |
| `--session-renew-margin` | `TUYA_SESSION_RENEW_MARGIN` | `24h` | Renew this long before expected expiry. |
| `--session-lifetime` | `TUYA_SESSION_LIFETIME` | `96h` | Assumed cookie lifetime when cookies carry no expiry. |
| `--credentials-file` | `TUYA_CREDENTIALS_FILE` | none | `KEY=VALUE` file with `TUYA_EMAIL`, `TUYA_PASSWORD`, optional `TUYA_REGION`; overrides the env vars below. |
| | `TUYA_EMAIL`, `TUYA_PASSWORD`, `TUYA_REGION` | none | Re-login credentials. Matched to the stored session by email (case-insensitive) and region (if set). |
| `--health-listen` | `TUYA_HEALTH_LISTEN` | off | Health endpoint address (`8556`, `:8556` -> `127.0.0.1:8556`). |

Keep the credentials file outside the repository and the data directory,
mode `0600`, owned by the service user (e.g. a systemd `LoadCredential=` or a
Docker secret).

## Build

Local (Go 1.26.4; `go.mod` requires >= 1.24):

```bash
CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -ldflags "-s -w -X main.VERSION=0.0.6-cvs.1" -o tuya-ipc-terminal .
go vet ./... && go test -race ./...
```

Docker (base images pinned by digest in the `Dockerfile`):

| Stage | Image |
|---|---|
| build | `golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648` |
| runtime | `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab` |

```bash
docker build -t tuya-ipc-terminal-cvs:0.0.6-cvs.1 .
```

The runtime image runs as uid/gid 65532, working directory and volume `/data`
(session state ends up in `/data/.tuya-data`). The binary inside the image is
byte-identical to the local build command above with the same Go version
(verified for `v0.0.6-cvs.1`, linux/amd64: sha256
`7573c105237bb946c6eb603fd5e80d5f146a3b97e4abb1d3e9edd659b0462c8a`). Tagged releases are published by CI to
`ghcr.io/practicaldev101/tuya-ipc-terminal-cvs:<version>`; deploy by digest.

## Run (loopback only)

```bash
# one-time, interactive (QR or password), stores ./.tuya-data
./tuya-ipc-terminal auth add eu-central user@example.com
./tuya-ipc-terminal cameras refresh

# service
TUYA_CREDENTIALS_FILE=/etc/tuya-bridge/credentials.env \
./tuya-ipc-terminal rtsp start --listen 127.0.0.1 --port 8554 \
  --media-timeout 20s --health-listen 8556

# recorder on the same host
ffmpeg -rtsp_transport tcp -i rtsp://127.0.0.1:8554/CORN_CHM-302/sd ...
curl -s http://127.0.0.1:8556/healthz
```

Docker: use host networking (WebRTC/ICE behind a bridge NAT is unreliable) and
a persistent volume:

```bash
docker run -d --name tuya-bridge --network host \
  -v tuya-data:/data \
  -v /etc/tuya-bridge/credentials.env:/run/secrets/tuya.env:ro \
  -e TUYA_CREDENTIALS_FILE=/run/secrets/tuya.env \
  tuya-ipc-terminal-cvs:0.0.6-cvs.1 \
  rtsp start --listen 127.0.0.1 --port 8554 --health-listen 8556
```

The first `auth add` / `cameras refresh` can be run with the same image and
volume (`docker run --rm -it --network host -v tuya-data:/data
tuya-ipc-terminal-cvs:0.0.6-cvs.1 auth add ...`).

## Known limitations

- **No RTP timestamp rewrite.** Packets are forwarded with the camera's RTP
  timestamps. The prototype needed regenerated timestamps and H.264
  re-encoding in FFmpeg; that stays the recorder's job.
- **SPS/PPS** are advertised in the SDP only once they have been seen in the
  stream; the very first client of a fresh session may still have to wait for
  the next IDR frame. Only H.264 gets `sprop-parameter-sets` (not HEVC).
- **Watchdog granularity:** checks run every `timeout/4` (max 5 s), so with
  the default 20 s a stall is acted on after 20-25 s. It watches video only.
  Teardown closes recorder connections on purpose; the recorder must
  reconnect with backoff (a dead camera means a reconnect every ~20 s).
- **Session expiry is assumed**, not read from Tuya: stored cookies carry no
  expiry (the cookie jar drops it), so 96 h after the last login is used,
  plus a live validation call every check. Re-login uses the same
  email/password API as `auth add --password` and the region's continent as
  country code, exactly like upstream. QR-only accounts (no password) cannot
  be renewed automatically. It is unknown whether a new login invalidates the
  MQTT/WebRTC session of a stream that is already running; running streams
  keep their session, new streams read the refreshed session from disk.
- **Health is on-demand:** streams only exist while an RTSP client is
  attached, so `/healthz` lists streams the recorder is currently pulling.
- Not validated yet against real hardware with these patches; long-run
  behaviour (24-72 h endurance, HD, five concurrent cameras) is untested.
- Logs are human-readable zerolog console lines at trace level (upstream
  default), including RTSP request/response dumps.
