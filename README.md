# engine

[![Docs](https://img.shields.io/badge/docs-hugo%20%2B%20hextra-0079A8)](https://go-wasmcontainers.github.io/docs/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-wasmcontainers/engine/actions/workflows/ci.yml/badge.svg)](https://github.com/go-wasmcontainers/engine/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-wasmcontainers/engine.svg)](https://pkg.go.dev/github.com/go-wasmcontainers/engine)
![status](https://img.shields.io/badge/status-M1-green)

**A container engine whose containers are WASI preview 1 (`wasip1`)
WebAssembly modules**, in pure Go (`CGO_ENABLED=0`): the daemon `wctrd`
serves an API compatible with a subset of the Docker Engine API, and the
client `wctr` ("wasm container") speaks the familiar container CLI grammar —
`pull`, `run`, `ps`, `logs`, `rm`, `rmi`, `inspect`, `images`. It is the
engine of the [go-wasmcontainers](https://go-wasmcontainers.github.io/)
training labs, which will run entirely in a web browser (see
[playground](https://github.com/go-wasmcontainers/playground)). Milestone
M1 runs natively on Linux and macOS, with [wazero](https://wazero.io) as the
executor, so the whole core is testable in CI. Documentation:
<https://go-wasmcontainers.github.io/docs/>.

> **Status: M1.** `wctrd` serves its API on a unix socket; `wctr` pulls
> images from an OCI image layout or an OCI registry and runs them as
> WebAssembly containers. No networking, no `exec`, no build yet — see
> [Limitations](#limitations).

## Demo

```console
$ go install github.com/go-wasmcontainers/engine/cmd/wctr@latest github.com/go-wasmcontainers/engine/cmd/wctrd@latest
$ export WCTR_HOST=unix://$HOME/wctr.sock    # the default, /var/run/wctr.sock, needs write access to /var/run
$ wctrd -H "$WCTR_HOST" &
wctrd v0.1.0 listening on unix:///home/me/wctr.sock (data root /home/me/.local/share/wasmcontainers)
```

With an OCI image layout `./layout` holding three images (`hello`, `greet`,
`probe`) — this exact sequence is the `TestAcceptance` test in `cmd/wctrd`,
run against the real binaries in CI:

```console
$ wctr pull oci:./layout:hello
Pulling from oci:/home/me/layout:hello
f0beefb89ff1: Pulling fs layer
f0beefb89ff1: Pull complete
Digest: sha256:da3eead2a4f462a53bebdee2f9dc5fcb94cc5a97ebe1680477f0ce0328ff67f0
Status: Downloaded newer image for hello:latest
$ wctr run --rm hello
Hello from a WebAssembly container!
$ wctr run --rm -e NAME=x greet
Hello, x!
$ wctr run --name failing probe exit 42
bye
$ echo $?
42
$ wctr ps -a
CONTAINER ID   IMAGE   COMMAND                  CREATED        STATUS                               PORTS   NAMES
2105748c99ce   probe   "/bin/probe.wasm exi…"   1 second ago   Exited (42) Less than a second ago           failing
$ wctr logs failing
bye
$ wctr rm failing
failing
$ wctr images
REPOSITORY   TAG      IMAGE ID       CREATED        SIZE
greet        latest   068beee4f451   12 hours ago   2.45MB
hello        latest   85c424cd2beb   12 hours ago   735kB
probe        latest   ae7fb9d7cefb   12 hours ago   811kB
```

Registries work the same way (`wctr pull localhost:5000/lab/hello:v1`,
Docker Hub names such as `user/app:tag`); `wctr run` pulls an image it does
not have.

## Commands

`wctr` (client). The daemon is `-H unix:///path`, else `$WCTR_HOST`, else
`unix:///var/run/wctr.sock`.

| Command | Flags |
|---|---|
| `pull IMAGE` | `IMAGE` is a registry reference or `oci:<dir>[:<ref.name>]` |
| `images` | `-q` |
| `run IMAGE [COMMAND] [ARG...]` | `--rm`, `-d/--detach`, `--name`, `-e/--env KEY=value` (or `KEY` from the shell), `--entrypoint`, `-w/--workdir`, `-m/--memory` |
| `ps` | `-a/--all`, `-q` |
| `logs CONTAINER` | `-f/--follow` |
| `rm CONTAINER...` | `-f/--force` |
| `rmi IMAGE...` | `-f/--force` |
| `inspect NAME\|ID...` | containers, then images |
| `kill CONTAINER...` | |
| `version` | |

Exit status of `wctr run`: the container's, or 125 (the CLI or daemon
failed), 126 (not a WebAssembly module), 127 (module not found), 137 (killed
or wall-clock timeout), 134 (the module trapped).

`wctrd` flags: `-H/--host unix:///path` (default `unix:///var/run/wctr.sock`),
`--data-root` (default `$XDG_DATA_HOME/wasmcontainers` or
`~/.local/share/wasmcontainers`), `--memory` (default linear-memory limit per
container, `256m`), `--run-timeout` (wall-clock limit per run, `10m`; `0` =
none), `--storage-quota` (bytes a container may write, `256m`),
`--max-containers` (`256`), `--insecure-registry host:port` (loopback
registries are always plain HTTP).

API endpoints (shapes of the Docker Engine API v1.47, `/v1.24/`…`/v1.47/`
prefixes accepted): `GET /_ping`, `GET /version`, `POST /images/create`
(`fromImage`, `tag`), `GET /images/json`, `GET /images/{name}/json`,
`DELETE /images/{name}`, `POST /containers/create`, `GET /containers/json`,
`POST /containers/{id}/start|wait|kill`, `GET /containers/{id}/logs`
(multiplexed stream), `GET /containers/{id}/json`,
`DELETE /containers/{id}`. Other clients of the Docker Engine API can use
these endpoints; this has not been tested with any of them yet, and
attaching to a container (what an interactive `run` does) is not
implemented.

## Images it runs

Both shapes, selected from an index by platform `wasip1/wasm`:

- the CNCF TAG-Runtime **wasm OCI artifact**: config
  `application/vnd.wasm.config.v0+json`, one `application/wasm` layer; the
  module is placed at `/<org.opencontainers.image.title>` (else
  `/module.wasm`) and is the entrypoint;
- an **ordinary OCI image** (OCI or Docker schema 2 media types) whose
  `tar` / `tar+gzip` layers hold a `.wasm` module named by the config's
  `Entrypoint`/`Cmd`; `argv[0]` is looked up like a shell does: absolute,
  relative to `WorkingDir`, or on `$PATH` (also trying `name.wasm`).

The module sees the container's root filesystem as its preopened `/`
(writable, copy-on-write over the image), `PWD` set to the image's
`WorkingDir` (default `/`), the image's `Env` plus `-e`, `PATH` and
`HOSTNAME` defaults, real clocks and `crypto/rand`.

## Packages

| Package | What it does |
|---|---|
| `image` | OCI content store (content-addressed, every blob sha256-verified), manifests, indexes, configs; pull from an OCI image layout or a registry's `/v2` API with token auth |
| `rootfs` | in-memory root filesystem: layers applied per the OCI layer spec (whiteouts, opaque whiteouts), frozen as a shared read-only lower, cloned copy-on-write per container |
| `runtime` | the `Executor` interface (no WebAssembly runtime type in it, so a browser executor can implement it too) |
| `runtime/wazero` | the native executor: wazero, WASI preview 1, memory limit, wall-clock timeout, compiled modules cached per sha256 |
| `container` | lifecycle `created` → `running` → `exited` (restartable), exit codes, bounded logs, wait conditions, names, ID prefixes |
| `daemon` | image store + unpacked image trees + container manager; the create override rules |
| `api`, `api/server` | the API: types, client, multiplexed stream; the HTTP server |
| `cmd/wctr`, `cmd/wctrd` | the CLI (cobra; also builds for `GOOS=wasip1`) and the daemon |

## Performance

Measured on an Apple M4 Max, one core (`-cpu 1`), go1.27.3; benchmarks are
committed (`go test -run '^$' -bench . -cpu 1 ./image ./rootfs ./runtime/wazero`):

| Benchmark | What | Median of 3 |
|---|---|---|
| `BenchmarkPullLayout` | cold pull of the `hello` image (735 kB tar+gzip layer) from an OCI layout: verify + store | 1.26 ms (583 MB/s) |
| `BenchmarkUnpackGzipLayer` | apply that layer (2.4 MB module) to an empty tree | 11.5 ms (210 MB/s, bound by `compress/gzip`) |
| `BenchmarkClone` | a container's copy-on-write root plus its first write | 2.2 µs |
| `BenchmarkStartCold` | first container of an image: wazero compiles the 2.4 MB Go module, runs it to exit | 610 ms |
| `BenchmarkStartWarm` | every later container: compiled module from the per-digest cache | 3.4 ms |

The daemon also keeps each image's unpacked tree (bounded, 16 images), so a
`wctr run` of a pulled image pays neither the unpack nor the compile again.
These numbers are not yet set against a reference runtime (wasmtime,
runwasi): none is installed on the measuring machine; that comparison is an
open item.

## Security

- **Untrusted content**: every manifest, index, config and blob is bounded
  in size (manifest/config 4 MiB, blob 512 MiB, image 1 GiB, 128 layers) and
  verified against its digest before use; a registry's
  `Docker-Content-Digest` header must match; `Authorization` never follows a
  redirect to another host; an https registry may not send the client to an
  http token realm.
- **Layers**: absolute names, `..`, writes under a symlink or a file, symlink
  targets that climb above the root, hard links to anything but an existing
  regular file are refused; total bytes (1 GiB), entries (131072), path
  length, depth and gzip ratio (200:1) are bounded; devices and FIFOs are
  skipped.
- **Containers**: the root filesystem is in memory and every path resolves
  inside it, so nothing reaches the host; linear memory, wall-clock time,
  written bytes, kept logs and the number of containers are bounded.
- **API**: served only on a unix socket (TCP is refused: the API has no
  authentication). The socket is bound inside a fresh `0700` directory, made
  `0600`, then renamed into place, so it is never reachable by others and
  the parent directory (e.g. `/var/run`) is never re-permissioned. Request
  bodies are bounded (1 MiB) and header reads time out. Whoever can open the
  socket can make the daemon read any OCI layout the daemon's user can read
  and contact any registry — that is the socket's owner, by construction.
- **Fuzzed** in CI (corpus committed under `testdata/fuzz`): the layer
  unpacker, the path resolver, the document parsers, the reference parser,
  and the client's demultiplexer. `govulncheck` runs on every change.

## Limitations

- No networking (the WasmEdge socket extension is a later milestone), no
  `exec` / process spawn (WASIX subset later), no TTY, no stdin attach.
- No `build`, volumes, compose, or Kubernetes API yet.
- Containers live in the daemon's memory: they do not survive a `wctrd`
  restart (images do).
- Layers: `tar+zstd` is not supported yet; hard links from image layers are
  separate copies once a container writes to them (as overlayfs does without
  its index feature).
- Registry auth is anonymous (Bearer token flow); no `login`.

## References

Every format and protocol is implemented from its specification; the list,
with what each one decides, is in [docs/REFERENCES.md](docs/REFERENCES.md).
Docker is a trademark of Docker, Inc.; this project is not affiliated with
it, and names it only to say which API it is compatible with.

## License

BSD 3-Clause, see [LICENSE](LICENSE).
