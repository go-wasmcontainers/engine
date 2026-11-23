# Audio output for containers: design

Status: the host module and `run --audio` are implemented (engine#71; package
`audio`); the page side and the Quake backend follow (playground#54,
go-quake1/engine#107). Tracked by go-quake1/engine#106 (item 2: Quake in a
container plays sound). Where the implementation settled a point the text below
leaves open: a non-finite sample, `+Inf` included, becomes 0 (section 3 is
followed; the test list in section 7 says 1 for `+Inf`, which contradicts it);
the ring memory bounds are enforced as the stream counts times the per-stream
maximum (2 x (128 KiB + 64 B) per container, 8 x per page).

A container granted audio writes PCM into a host module; the playground page
plays it through an AudioWorklet. This document fixes the guest-facing import,
the grant and its bounds, the browser side, the latency budget and the tests.
Nothing here is implemented yet; the implementation issues are listed at the
end.

## 1. Where it lives, and why

The host module goes in **this repository** (`go-wasmcontainers/engine`), as a
new package `audio`, beside `wasi.WASIX`:

- The grant (`run --audio`, the `"audio"` device capability) is the engine's:
  `api/devices.go` (`KnownCaps`, `DeviceCaps`), `cli/cli.go` and the
  persisted `container.Caps` already carry `"gpu"`. The module and its grant
  should be versioned together.
- go-wasmcontainers/gpu is the home of modules that need a GPU and the
  go-gfx stack. Audio needs neither; putting it there would make every
  audio-only consumer depend on WebGPU.
- Its only new dependency is `github.com/wasmdesk/wasmbox/audioring`
  (BSD-3-Clause, the same licence as this repository): a tiny,
  platform-neutral, fuzzed SPSC ring. A new repository would hold about
  300 lines and one dependency; that is not worth a separate release train.
  (Decided: this engine, not a new `go-wasmcontainers/audio`.)

The page side (AudioContext, worklet node, unlock) goes in
go-wasmcontainers/playground (`web/app.js`, `cmd/engine/main_js.go`), like the
GPU surfaces. The worklet source itself stays in ONE place: it moves from
wasmbox's repository root into the `audioring` module as an embedded asset, so
wasmbox, the playground and anything later serve the same file.

## 2. Prior art (what was opened)

- **No WASI audio proposal exists.** The WASI proposal list
  (`WebAssembly/WASI`, `docs/Proposals.md`, read 2026-11-16) has no audio,
  sound, PCM or media entry in any phase; the only graphics one is WebGPU
  (phase 2). The `wasi-gfx` organisation's repositories hold none either.
  The module is therefore a private import module, like `wasi_webgpu_batch`,
  and is kept small so that it can be lowered onto a standard one if a
  proposal appears.
- **`wasmdesk/wasmbox/audioring`** (`ring.go`, `js_wasm.go`) and
  **`audio-worklet.js`**: interleaved float32 in a SharedArrayBuffer, a
  64-byte header of 16 Int32 words (write, read, underruns, played, overruns,
  state), power-of-two capacity, free-running u32 counters, release/acquire
  through `Atomics`; bounds 8000..192000 Hz, 1..2 channels, 256..65536
  frames. The worklet resamples linearly, primes two quanta before
  (re)starting, zero-pads and counts an underrun once per starvation. Writes
  never block; frames that do not fit are dropped and counted.
- **go-quake1/engine PR #47** (`backend/wasmbox/ringaudio.go`, `RingAudio`):
  the engine's 11025 Hz stereo int16 mix converted to float32 into a
  1024-frame ring (93 ms). **wasmbox `test/probe-audio.mjs`**: headless
  Chromium with `--autoplay-policy=document-user-activation-required`, a
  trusted click, "played about 2 s worth in a 2 s window", and a planted
  negative (writer disabled: played stays 0, the same assertions must fail).
- **go-quake1/engine README, "Play in a browser (wasm)"**: `backend/wasm`
  plays through WebAudio (`AudioBufferSourceNode` per chunk); in a worker,
  which has no `AudioContext`, the ring is used and `WebAudio` is only the
  fallback.
- **This engine's host modules** (`wasi/hostmod.go`): a `HostModule` declares
  core-wasm functions; one `Instance` per process, opened at its first call,
  closed with the process; `Proc.HasCap`, `Proc.CapValue`, `Proc.Emit`,
  `Proc.Label`. In the browser a guest's host call is a message to the engine
  worker while the guest worker blocks in `Atomics.wait`
  (`runtime/browser/worker.js`), so **every call costs one round trip**.
- **go-wasmcontainers/gpu `gpubatch`**: the model for the grant and its
  quota (`Open` reads `"gpu"` and `"gpu-memory=BYTES"`, fails closed on a
  malformed grant; a nil `Provider` answers `Unsupported`; sanctions are
  reported with `Proc.Emit`).
- **W3C Web Audio API 1.1** (`w3.org/TR/webaudio-1.1`, §1.2 `AudioContext`):
  a context is "allowed to start" only if the user agent allows the
  transition from suspended to running, which it may tie to sticky
  activation; a new context always begins suspended. The render quantum is
  128 frames by default (`renderSizeHint`). `latencyHint` "interactive" is
  the default; `baseLatency` and `outputLatency` report the platform part of
  the latency.
- **Quake's mixer, as ported**: the README's status table still says `sound`
  is "pending", but `sound/` holds the channel pool, `Paint` (snd_mix),
  spatialisation and the WAV loader; `host/sound.go` dispatches
  `StartSound`/`AmbientSound`; `runloop` paints and calls
  `Backend.QueueAudio` every tic. `cmd/quake-wctr`'s backend
  (`backend/wasisurface`) embeds `backend.NoAudio`, so the container is
  silent by construction.

## 3. The guest-facing import

Import module **`wctr_audio_out`**: not `wasi_`, because no WASI proposal
backs it and the name must not squat that namespace. Core-wasm functions, every
parameter an `i32`, every result an `Errno` (`i32`). Output only: there is no
capture, and none is implied by this grant.

| Function | Parameters | Effect |
|---|---|---|
| `stream_open` | `rate, channels, capacity, out` | open a stream; its handle (u32) is stored at `out` |
| `stream_write` | `stream, ptr, frames, out` | queue `frames` frames read from `[ptr, ptr + frames*channels*4)`; stores `{accepted u32, queued u32}` (8 bytes) at `out` |
| `stream_stats` | `stream, out` | stores `{played u32, underruns u32, refused u32, state u32}` (16 bytes) at `out` |
| `stream_close` | `stream` | close the stream (also done when the process ends) |

**Sample format.** Interleaved IEEE-754 float32, little endian, nominal range
[-1, 1]. One format only: it is what the worklet and Web Audio use, and it
keeps the module free of format conversion. A guest that mixes in int16
(Quake) divides by 32768, as `RingAudio` does.

**Rate and channels: negotiation by refusal.** The guest asks for the rate it
mixes at; the host accepts any integer rate in [8000, 48000] and any channel
count in {1, 2} **as is** (the worklet resamples to the context's rate, so the
guest never needs to know the hardware rate), or answers `InvalidArgument`.
There is no "the host picked another rate" path: a guest cannot be surprised
by a rate it did not ask for, and nothing about the output device leaks.

**Buffer ownership.** The guest owns `[ptr, ...)`; the host copies it during
the call and keeps no reference. The ring is host memory (a SharedArrayBuffer
shared by the engine worker and the AudioWorklet); the guest never sees it,
so a guest cannot corrupt the indices the worklet trusts. The copy also
sanitises: a non-finite sample becomes 0 and every sample is clamped to
[-1, 1].

**Writes never block.** `stream_write` accepts `min(frames, free)` frames and
reports how many, plus the frames now queued. `frames = 0` is a poll (one call
per tic does both). Frames not accepted are the guest's to retry or drop; the
host counts them in `refused`.

**Before the page plays.** Until the worklet runs (no user gesture yet, no
page attached, a native host with a provider that discards), the stream is in
state `waiting`: writes are accepted and **discarded**, and `queued` reads 0.
Sound produced before the gesture is never played late. The host knows the
worklet runs from a heartbeat word the worklet advances every quantum (header
word 6, an addition to the audioring layout); a heartbeat older than 200 ms
puts the stream back in `waiting`.

`state`: 0 waiting, 1 playing, 2 closed by the host (process ended, grant
revoked, page gone).

**Pacing (what a guest should do).** A guest keeps `L` seconds queued and
paints, per tic, `min(elapsed * rate, L * rate - queued)` frames, never more
than it can write. This is tyrquake's `S_Update` (`snd_dma.c`: paint up to
`soundtime + _snd_mixahead * speed`, with `soundtime` from
`SNDDMA_GetDMAPos`): `queued` plays the part of the DMA position, `elapsed`
keeps time advancing while the stream waits.

Errno values (shared numbering with `wasi_webgpu_batch` where the meaning is
the same):

| Errno | Value | When |
|---|---|---|
| Success | 0 | |
| InvalidArgument | 1 | bad handle; rate, channels or capacity out of bounds; capacity not a power of two |
| Denied | 2 | the process has no `audio` capability |
| Unsupported | 3 | no audio provider here (native `wctr` today) |
| TooLarge | 4 | `frames` above the stream's capacity |
| QuotaExceeded | 5 | too many streams in the container or on the page |
| Fault | 9 | a range outside guest memory (computed in 64 bits) |

## 4. The grant and its bounds

`wctr run --audio IMAGE`: a `DeviceRequest{Count: -1, Capabilities:
[["audio"]]}` (Compose: `deploy.resources.reservations.devices` with
`capabilities: [audio]`). `KnownCaps` becomes `{"gpu", "audio"}`; `DeviceCaps`
refuses any option on an audio request. The capability persists with the
container like `"gpu"`. `--audio` is independent of `--gpu`.

| Bound | Value | Why |
|---|---|---|
| rate | 8000..48000 Hz | 48 kHz is every browser's common output rate; above it a guest only buys resampler work |
| channels | 1..2 | the worklet's outputs |
| capacity | 256..16384 frames, a power of two | audioring's index arithmetic; 16384 frames is 1.5 s at 11025 Hz, 341 ms at 48 kHz |
| streams | 2 per container, 8 per page | a worklet node per stream costs audio-thread time; Quake needs 1 (music is mixed in the guest) |
| ring memory | at most 128 KiB + 64 B per stream, 256 KiB per container, 1 MiB per page | follows from the above; host memory, not counted in `--memory` |
| copy per call | at most 128 KiB | `frames` above capacity is `TooLarge` |

The module emits `audio open`, `audio close` and `audio refused` events
(`Proc.Emit`) so `wctr events` shows them.

**What a hostile guest can do, and what stops it.**

- *Make noise.* Only once the user granted `--audio` and gave the page a
  gesture. Samples are clamped; the page puts a gain node per container
  between the worklet and the destination: volume and mute per container in
  its dashboard pane, plus a page-wide mute.
- *Corrupt the ring.* It cannot reach it: the guest only passes a pointer
  into its own memory, which is copied. The worklet already resyncs on an
  impossible header (`avail > cap`).
- *Exhaust memory or audio-thread time.* Streams, capacity and rate are
  bounded per container and per page; a refusal is `QuotaExceeded`, never a
  partial allocation.
- *Overflow a range.* `ptr + frames*channels*4` is computed in 64 bits
  against the memory size before any copy (`wasi.CheckBlock`'s pattern);
  `frames*channels*4` cannot exceed 128 KiB once `frames <= capacity`.
- *Busy-loop on `stream_write`.* Each call is one engine round trip, the same
  cost as any WASI call (`clock_time_get` in a loop); the copy per call is
  bounded. Nothing new is exposed.
- *Learn something.* `state` tells whether the page is playing (so, whether a
  gesture happened); `played` advances once per 128-frame quantum, a clock no
  finer than the one `clock_time_get` gives. The output device's rate and
  latency are deliberately not reported.
- *Capture.* There is no input path. A microphone would be a separate
  capability and a separate design.

## 5. The browser side

```
guest worker ──stream_write──▶ engine worker (Go)          page (main thread)          audio thread
  (Atomics.wait)   round trip   audio.Browser provider      app.js                      AudioWorkletProcessor
                               copies + sanitises into  ──▶ {t:"audio",op:"open",  ──▶  reads the ring,
                               the SharedArrayBuffer ring    sab, rate, ch, cap}         resamples, heartbeat
```

- **Engine worker** (`cmd/engine/main_js.go`): `audio.Browser{Post: postAudio}`
  is the module's provider. `Open` calls `audioring.Open` with that `Post`,
  which allocates the ring and posts `{t:"audio", op:"open", stream,
  container, sampleRate, channels, capacityFrames, sab}`; close stores the
  ring's closed state and posts `{t:"audio", op:"close", stream}`. The page
  is cross-origin isolated already (`web/app.js` refuses to run without
  `crossOriginIsolated`), so SharedArrayBuffer is available.
- **The feed does not go through the engine worker.** The engine worker is
  one thread shared by Go's garbage collector (slices of 60-143 ms measured
  on a loaded CI runner) and every host call of every container, so a
  `stream_write` served there waits behind them and the game's feed arrives
  late (the page's worklet starves). `worker.js` therefore serves
  `stream_write` and `stream_stats` itself, on the guest's own thread, for a
  stream whose ring it allocated: `stream_open` allocates the
  SharedArrayBuffer in the guest worker (a worker blocked in `Atomics.wait`
  runs no event loop, so a buffer cannot be handed to it later), posts it to
  the engine just ahead of the call (`{t:"audioring", sab}`, parked in
  `self.__wctrAudioRings[container]`), and `audio.Browser.Open` adopts it and
  marks header word 8. The semantics are section 3's, call for call (errnos in
  the same order, waiting / playing / closed, sanitising). The worker counts
  the frames it refuses in header word 7; `audio.OutOfBand` lets `stream_stats`
  and the `refused` event report them. `stream_open` and `stream_close` stay
  the engine's (grant, quotas, events); a ring the engine did not adopt is
  served by the engine as before. Test: `TestAudioFeedDoesNotWaitForTheEngine`
  (an engine that stalls 150 ms per call).
- **Page** (`web/app.js`): on the first `open`, create one
  `AudioContext({latencyHint: "interactive"})` and `addModule` the worklet
  (served from the embedded `audioring` asset). Per stream: an
  `AudioWorkletNode` with `processorOptions {sab, sampleRate, channels,
  capacityFrames}` and `outputChannelCount: [2]`, into a per-container
  `GainNode`, into the destination. wasmbox passes the SharedArrayBuffer
  the same way and its headless probe plays through it.
- **Autoplay unlock.** The context starts suspended (Web Audio §1.2). The
  page adds capturing `pointerdown` and `keydown` listeners on `window` that
  call `resume()`; they stay until the state is `running`, and come back on a
  `statechange` to `suspended` or `interrupted` (Safari). The click that
  focuses Quake's pane is that gesture. If the context is created after a
  gesture already happened, sticky activation lets it start at once.
- **Worklet changes** (in `audioring`'s copy, used by wasmbox too): advance
  the heartbeat word every `process()` call. Nothing else changes.
- **Native `wctr`**: no provider, `Unsupported` (as gpubatch). A native
  output is out of scope: a deliberate limit of this design, not a gap
  to fill silently.

## 6. Latency

| Stage | Cost |
|---|---|
| guest mix-ahead `L` | chosen by the guest: tyrquake's `_snd_mixahead` is 0.1 s; two frame intervals (about 35 ms at 60 Hz) is the floor that survives one late frame |
| `stream_write` round trip | one engine message per tic; unmeasured, the e2e test records it |
| worklet prime | two quanta of output before the first sample (5.3 ms at 48 kHz) |
| render quantum | 128 frames, 2.7 ms at 48 kHz |
| platform | `baseLatency + outputLatency`, logged by the e2e test |

Expected total: `L` + about 8 ms + the platform's output latency. The ring
capacity only bounds `L` from above; the guest's pacing, not the ring size,
sets the latency. 1024 frames (93 ms at 11025 Hz), as in wasmbox, is enough
for Quake with `L` = 50 ms.

## 7. Tests

**Engine, native** (`audio` package, a fake provider over audioring's
in-memory backing, `wasi/hostmodtest`):

- every refusal of §3 and §4 by table, each with its errno (rate 7999 and
  48001, channels 0 and 3, capacity 255, 300 and 32768, a third stream,
  a ninth page stream, an unknown handle);
- `Denied` without the capability, `Unsupported` with a nil provider;
- `Fault` at the 32-bit edge (`ptr = 0xFFFFFFF0` with a large `frames`), so
  the range check is proven to be done in 64 bits;
- `accepted`/`queued` against a model consumer; `refused` counts what did not
  fit; a NaN, +Inf and 2.0 come out of the ring as 0, 1 and 1;
- `waiting`: before the first heartbeat, and after a heartbeat older than
  200 ms, writes are discarded and `queued` is 0;
- the stream is closed (ring state closed, `close` posted) when the process
  ends, including during a pending call;
- `FuzzCall` over the four functions, corpus committed.

**api/cli**: `DeviceCaps` grants `"audio"` and refuses an option on it;
`run --audio` reaches `Proc.HasCap("audio")`; the grant survives a restart.

**Playground, headless Chromium** (e2e, launched with
`--autoplay-policy=document-user-activation-required`, as wasmbox's probe):
a `tone` image writes a 440 Hz sine, 11025 Hz stereo, amplitude 0.25, paced as
in §3. The page exposes `window.__wctrAudio.stats()` per stream: context
state, `played`, underruns, and from an `AnalyserNode` tapped after the
worklet the peak frequency and the RMS. The test:

1. runs `wctr run --rm --audio tone`;
2. asserts the context is `suspended` before any gesture (otherwise the
   gesture check is vacuous);
3. clicks with WebDriver *Perform Actions* (a trusted gesture; the
   playground's `internal/webdriver` gains that call);
4. over a 2 s window asserts `played` is at least half of 2 s worth, the peak
   is the 440 Hz bin (plus or minus one), the RMS is above -30 dBFS, and
   underruns stay under a stated bound.

Planted negatives, each run in the same test, each required to FAIL the same
assertions with the expected reason (a negative that passes fails the test):

- `tone --silent` (zeros): `played` advances, the RMS check fails, which
  proves the RMS check can see silence;
- `tone --hz 880`: the frequency check fails;
- the same image without `--audio`: `Denied`, `played` stays 0;
- no click: the context stays `suspended`, `played` stays 0.

**Quake** (`cmd/quake-wctr`): a native test over a fake host that, for
simulated tics of irregular length, checks the frames written track
`elapsed * 11025` within one tic and never queue more than `L`; the
playground's `TestQuake` asserts `played > 0` after its click.

## 8. Found while reading: Quake's mixer is not paced

`runloop` paints `len(MixBuffer)` = `sound.MixBufferStereoFrames` = 512 frames
every tic, whatever the tic's length (`runloop/runloop.go`, step 8;
`runloop/setup.go`). tyrquake paints up to the DMA position plus
`_snd_mixahead` (`snd_dma.c`, `S_Update_`). At 60 tics a second that is about
30700 frames a second for an 11025 Hz output, 2.8 times real time: sounds are
consumed 2.8 times too fast and the wasmbox ring drops the excess. This is
read from the source, not measured; it is filed with the quake-wctr step,
because the pacing of §3 is its fix.

## 9. Implementation steps

1. `wasmdesk/wasmbox`: `audioring` serves the worklet as an embedded asset
   and adds the heartbeat word.
2. `go-wasmcontainers/engine`: the `audio` host module, the `"audio"`
   capability and `run --audio`.
3. `go-wasmcontainers/playground`: provider wiring, page AudioContext and
   unlock, the `tone` image and the e2e test.
4. `go-quake1/engine`: the paced mixer and a `quake-wctr` audio backend over
   `wctr_audio_out`. The `quake` image does not ask for audio by default:
   the user adds `--audio`, and without it the game runs silent.

## Decisions (2026-11-16)

- **Home**: a package of this engine, not a new repository.
- **Name**: `wctr_audio_out`. No WASI proposal backs it, so it does not
  take a `wasi_` name.
- **Native `wctr`**: answers `Unsupported`. This is a deliberate limit of
  this design; a native output would need its own design.
- **Mute and volume**: both levels. Each container has volume and mute in
  its dashboard pane, and the page has a global mute.
- **Quake**: the playground's `quake` image is not run with `--audio` by
  default; the user adds it.
