# References

Every behaviour in this engine that follows an external format or protocol is
written against one of the documents below. Each entry says which version was
opened and which part of the code it governs. Nothing is cited that was not
read.

## OCI Image Format Specification, v1.1.1

Read from the module source `github.com/opencontainers/image-spec@v1.1.1`
(the Markdown files that make up the specification).

| Document | Used by | What it decides |
|---|---|---|
| `descriptor.md` | `image/digest.go`, `image/types.go` | digest grammar; `sha256` MUST be supported and its encoded part MUST match `[a-f0-9]{64}` (upper case forbidden); the `data` field MUST be identical to the referenced content and SHOULD be verified |
| `manifest.md` | `image/parse.go` | `schemaVersion` MUST be 2; `layers[0]` is the base and the final filesystem MUST equal applying the layers in order; unknown `artifactType`/`mediaType` MUST NOT be an error when storing |
| `image-index.md` | `image/parse.go`, `image/select.go` | `manifests` MUST be present (MAY be empty); per-entry `platform` (`os`, `architecture`) used for selection; unknown media types MUST NOT be an error |
| `config.md` | `image/types.go` | `architecture`, `os`, `rootfs` REQUIRED; `config.Entrypoint`, `Cmd`, `Env`, `WorkingDir`; unknown fields MUST NOT be an error |
| `layer.md` | `rootfs/unpack.go` | tar changesets; whiteouts (`.wh.` prefix) apply only to lower layers, are hidden once applied; opaque whiteout `.wh..wh..opq` is applied first regardless of its position in the archive; both explicit and opaque forms MUST be accepted; hardlinks (type `1`) and the note that union filesystems may not preserve them across copy-up |
| `image-layout.md` | `image/layout.go` | `oci-layout` MUST exist and carry `imageLayoutVersion`; `index.json` MUST be an image index; `blobs/<alg>/<encoded>` MUST match its digest; `org.opencontainers.image.ref.name` on `index.json` descriptors names a tag |

## OCI Distribution Specification

Read from `https://raw.githubusercontent.com/opencontainers/distribution-spec/main/spec.md`.

- Pull a manifest: `GET /v2/<name>/manifests/<reference>`, the client SHOULD send
  an `Accept` header listing the manifest types it supports; `Content-Type`
  of the response is the manifest's type; if `Docker-Content-Digest` is used
  the client MUST verify it matches the content (`image/registry.go`).
- Pull a blob: `GET /v2/<name>/blobs/<digest>`; `404` when absent.
- Registries MAY redirect; clients SHOULD follow and MUST NOT forward the
  `Authorization` header across host boundaries (`image/registry.go`,
  `CheckRedirect`).
- `<name>` grammar `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*`
  and tag grammar `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}` (`image/reference.go`).
- Error body: `{"errors":[{"code","message","detail"}]}` (`image/registry.go`).

## Docker Registry token authentication

Read from `https://raw.githubusercontent.com/distribution/distribution/main/docs/content/spec/auth/token.md`.

- The `WWW-Authenticate: Bearer realm="…",service="…",scope="…"` challenge;
  the token request `GET <realm>?service=…&scope=…`; the response's `token`
  (also accepted as `access_token`), `expires_in`, `issued_at`
  (`image/registry.go`).

## Docker Engine API v1.47

Read from the OpenAPI document `https://docs.docker.com/reference/api/engine/version/v1.47.yaml`
(the HTML page at `/reference/api/engine/version/v1.47/` only links to it).

- Schemas reproduced (as a subset) in `api/types.go`: `ImageSummary`,
  `ContainerSummary`, `ContainerState`, `ContainerCreateResponse`,
  `ContainerWaitResponse`, `ImageDeleteResponseItem`, `SystemVersion`,
  `ErrorResponse` (`{"message": …}`).
- `GET /_ping` returns `OK` with an `Api-Version` header.
- `POST /images/create?fromImage=…&tag=…` streams JSON progress messages.
- `GET /containers/{id}/logs` with `follow`, `stdout`, `stderr`; without a TTY
  the body is the multiplexed stream: frames headed by
  `[8]byte{STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4}`, type 1 stdout,
  2 stderr, size big-endian (`api/stdcopy.go`).
- `POST /containers/{id}/wait` (`condition` defaults to `not-running`),
  `DELETE /containers/{id}` (`force`; 409 for a running container).
- `POST /containers/{id}/start` (204, 304 when already started),
  `POST /containers/{id}/stop` (204, 304 when already stopped; `signal` and
  `t` — a wasm guest has no signals, so stop kills at once),
  `POST /containers/{id}/restart` (204). `POST /containers/{id}/attach`
  (`stream`, `stdout`, `stderr`, `logs`): Docker hijacks the connection;
  without stdin to carry, this engine answers an ordinary streamed
  multiplexed response, which `start -a` reads.
- `GET /events` (`since`, `until`, `filters`), one `EventMessage` per line:
  `Type`, `Action`, `Actor{ID, Attributes}`, `scope`, `time`, `timeNano`;
  the actions containers, images and volumes report. Filters implemented:
  `type`, `event`, `container`, `image`, `volume`, `label`; others are
  refused, not ignored. `since`/`until` take Unix seconds (with a
  fraction), RFC 3339, or a duration back from now.
- `GET /system/df` (`SystemDataUsageResponse`: `LayersSize`, `Images`
  [`ImageSummary`], `Containers` [`ContainerSummary`], `Volumes`,
  `BuildCache`); `POST /containers/prune` (`ContainersDeleted`,
  `SpaceReclaimed`); `POST /images/prune` with `filters` `dangling=<bool>`
  (`false`: every unused image) answering `ImagesDeleted`
  [`ImageDeleteResponseItem`] and `SpaceReclaimed`. Unsupported filters are
  refused, not ignored.
- CLI reference, `docker system df` (columns `TYPE TOTAL ACTIVE SIZE
  RECLAIMABLE`; `-v` lists images, containers, volumes) and `docker system
  prune` (stopped containers and dangling images; `-a` every image without
  a container; the `Are you sure you want to continue? [y/N]` question
  unless `-f`; `Total reclaimed space:`).

## CNCF TAG-Runtime: Wasm OCI Artifact layout

Read from `https://tag-runtime.cncf.io/wgs/wasm/deliverables/wasm-oci-artifact/`.

- Config media type `application/vnd.wasm.config.v0+json`, layer media type
  `application/wasm`, manifest `application/vnd.oci.image.manifest.v1+json`.
- Config fields `created`, `author`, `architecture` (`wasm`), `os` (`wasip1`
  or `wasip2`), `layerDigests`, `component` (wasip2 only).
- Consumers should reject an artifact with more than one layer for this
  version; layer 0 is the entrypoint (`image/parse.go`, `rootfs/unpack.go`).

## wazero v1.12.0

Read from the module source `github.com/tetratelabs/wazero@v1.12.0`.

- `config.go`: `RuntimeConfig.WithMemoryLimitPages`, `WithCompilationCache`,
  `WithCloseOnContextDone` (terminates execution when the context is done and
  raises `sys.ExitError`); `ModuleConfig.WithArgs`, `WithEnv`, `WithFSConfig`,
  `WithName("")` for several instances, `WithSysWalltime`, `WithSysNanotime`,
  `WithRandSource`.
- `fsconfig.go`: `WithFSMount` (read-only `fs.FS`, not jailed) versus
  `WithSysFSMount` (writable `experimental/sys.FS`).
- `experimental/sys/fs.go`, `file.go`, `dir.go`, `oflag.go`,
  `unimplemented.go`: the writable filesystem interface the container root is
  adapted to (`runtime/wazero/sysfs.go`): `Errno` results, `Readdir(n)`
  semantics (no `io.EOF`), `Oflag` values.
- `imports/wasi_snapshot_preview1`: `Instantiate` the host module once per
  runtime; `sys.ExitError.ExitCode` carries `proc_exit`.

## Docker reference normalisation and display helpers

- `github.com/distribution/reference@v0.6.0`, `normalize.go`
  (`splitDockerDomain`, `defaultDomain = "docker.io"`, `officialRepoPrefix =
  "library/"`, `legacyDefaultDomain = "index.docker.io"`, the note that Docker
  Hub is actually served by `registry-1.docker.io`): `image/reference.go`.
- `github.com/docker/go-units@v0.5.0`, `duration.go` (`HumanDuration`) and
  `size.go` (`HumanSizeWithPrecision`, `RAMInBytes`): imported as is by the
  CLI and the server for the CREATED / STATUS / SIZE columns and `--memory`.

## cobra shell completion

Read from `github.com/spf13/cobra@v1.10.2`, `site/content/completions/_index.md`
and `completions.go`: the hidden `__complete` command (the word being
completed is the last argument, `""` for a new word), its output (one
`value\tdescription` per line, then `:<directive>`), `ValidArgsFunction`
(called after cobra has parsed flags and arguments; it does not filter by
prefix), `RegisterFlagCompletionFunc`, `CompletionWithDesc`,
`FixedCompletions`, `NoFileCompletions`, the directive bits (Error 1,
NoSpace 2, NoFileComp 4, FilterFileExt 8, FilterDirs 16, KeepOrder 32) and
`CompletionOptions.DisableDefaultCmd`. `cli/complete.go` and `cli.Complete`
are written against it.

## Go `wasip1` port

Read from `$(go env GOROOT)/src/syscall/fs_wasip1.go` (go1.27): the initial
working directory of a `wasip1` Go program is taken from `$PWD`, otherwise
the first preopen. The engine therefore sets `PWD` to the image's
`WorkingDir`. The same file's `openat` derives `path_open` rights from the
open mode (`fd_write` only when writing) and retries a directory with
`OFLAG_DIRECTORY`; package `wasi` follows those rights.

The import list package `wasi` implements is every
`//go:wasmimport wasi_snapshot_preview1` in go1.27's `runtime`
(`os_wasip1.go`, `lock_wasip1.go`), `syscall` (`fs_wasip1.go`,
`syscall_wasip1.go`, `os_wasip1.go`, `net_wasip1.go`) and
`internal/syscall/unix` (`at_wasip1.go`, `utimes_wasip1.go`), plus the rest
of the snapshot's functions (answered `ENOSYS`/`ENOTSUP`).

## WASI preview 1 (wasi_snapshot_preview1)

Read from `https://raw.githubusercontent.com/WebAssembly/WASI/snapshot-01/phases/snapshot/docs.md`
(the `main` branch no longer carries it): every function signature, the
`errno` enum in order (success = 0 … notcapable = 76), `rights` bits,
`filetype`, `fdflags`, `oflags`, `lookupflags`, `fstflags`, `whence`, and
the layouts of `iovec` (8), `fdstat` (24), `filestat` (64), `dirent` (24,
name follows, the last entry may be truncated), `prestat` (8),
`subscription` (48, union contents at 16) and `event` (32). Package `wasi`
is written against it.

## Threads proposal: shared memory and its binary encoding

Read from `https://raw.githubusercontent.com/WebAssembly/threads/main/proposals/threads/Overview.md`:
limits flags `0x00 min`, `0x01 min max`, `0x03 min max shared` (a shared
memory always has a maximum); in the JS API a shared memory requires
`maximum`, and its `buffer` is a new `SharedArrayBuffer` whenever the size
changed. `wasmbin.LimitMemory` writes `0x03` with the container's limit, and
the browser executor fetches `buffer` on every access.

## Browser platform

- MDN, `SharedArrayBuffer` (security requirements): shared memory needs a
  secure, cross-origin isolated context; without the COOP/COEP headers the
  constructor is hidden and `postMessage` throws for it.
- MDN, `Atomics.wait`: only on an `Int32Array`/`BigInt64Array` over a
  `SharedArrayBuffer`, never on the main thread; returns `ok`, `not-equal`
  or `timed-out`. The container worker blocks there while the engine
  serves its WASI call, and sleeps there for `poll_oneoff`.
- MDN, `WebAssembly.Module`: a compiled module can be posted to workers and
  instantiated several times; the executor compiles once per digest and
  memory limit and posts the module to each container's worker.
- V8 blog, "Code caching for WebAssembly developers" (v8.dev/blog/
  wasm-code-caching): only `compileStreaming`/`instantiateStreaming` are
  cached, keyed by the response's URL, for modules over 128 kB, written once
  TurboFan has finished; a new `200` for the URL or a new V8 version
  invalidates it. The article speaks of the HTTP cache only, so whether a
  `Response` from Cache Storage reaches the code cache is measured, not
  assumed (`runtime/browser` reports each compile's source and time).
- MDN, Storage quotas and eviction criteria: best-effort data is evicted
  LRU under pressure, all of an origin at once; `navigator.storage.persist()`
  prompts in Firefox and is decided by heuristics in Chromium and Safari;
  `estimate()` is an estimate; IndexedDB and Cache Storage writes over quota
  fail with `QuotaExceededError` (mapped to `state.ErrNoSpace`).
- MDN, Web Locks API: per-origin named locks, across tabs and workers,
  held until the callback's promise settles; `ifAvailable` fails at once
  instead of queueing, `steal` takes it over.
- web.dev, "The service worker lifecycle": an updated worker (byte
  different) installs, then waits while the old one controls a page;
  `registration.waiting`, `updatefound`, `controllerchange`; `skipWaiting()`
  activates at once, which is why the playground asks before switching.
