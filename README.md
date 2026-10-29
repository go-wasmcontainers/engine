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

> **Status: M1, plus the browser executor.** `wctrd` serves its API on a
> unix socket; `wctr` pulls images from an OCI image layout or an OCI
> registry and runs them as WebAssembly containers. The same engine runs in
> a web page ([In a browser](#in-a-browser)). Containers have a network:
> TCP and UDP between containers, by name, on in-engine networks or on a
> class network through the [relay](https://github.com/go-wasmcontainers/relay)
> ([Networks](#networks)). No `exec`, no build yet — see
> [Limitations](#limitations).

## Demo

```console
$ go install github.com/go-wasmcontainers/engine/cmd/wctr@latest
$ go install github.com/go-wasmcontainers/engine/cmd/wctrd@latest
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
| `run IMAGE [COMMAND] [ARG...]` | `--rm`, `-d/--detach`, `--name`, `-e/--env KEY=value` (or `KEY` from the shell), `--entrypoint`, `-w/--workdir`, `-m/--memory`, `-v/--volume`, `--network NAME` (default `bridge`; `none`: no network), `--network-alias NAME` (more names on a user-defined `--network`; replicas may share one), `-p/--publish [ip:][hostPort:]containerPort[/tcp\|udp]` (ip defaults to **127.0.0.1**; ranges as Docker's), `-P/--publish-all`, `--expose PORT[/proto]` — see [Published ports](#published-ports); `--restart no\|always\|unless-stopped\|on-failure[:N]` (see below; not with `--rm`) |
| `port CONTAINER [PRIVATE_PORT[/PROTO]]` | where a running container's ports are published (`80/tcp -> 127.0.0.1:49153`) |
| `ps` | `-a/--all`, `-q`; PORTS as Docker writes it (`127.0.0.1:8080->80/tcp, 9000/tcp`) |
| `logs CONTAINER` | `-f/--follow` |
| `rm CONTAINER...` | `-f/--force` |
| `rmi IMAGE...` | `-f/--force` |
| `commit CONTAINER [REPO[:TAG]]` | `-m/--message`, `-a/--author`, `-c/--change` (`CMD`, `ENTRYPOINT`, `ENV`, `EXPOSE`, `LABEL`, `ONBUILD`, `STOPSIGNAL`, `USER`, `VOLUME`, `WORKDIR`): the container's changes as one OCI layer (OCI whiteouts) on the parent's, a history entry (`empty_layer` when nothing changed); without a name, a `<none>` image addressable by ID (`POST /commit`) |
| `tag SOURCE TARGET` | another name for an image; a tag that moves leaves the old image as `<none>` (`POST /images/{name}/tag`) |
| `build`, `compose` | from [build](https://github.com/go-wasmcontainers/build) and [compose](https://github.com/go-wasmcontainers/compose), wired in `cmd/wctr` through `cli.Options.Extra`; they use wctr's `-H`. The root command reads its own flags before the subcommand (`TraverseChildren`), so `-f` is `compose`'s `--file` and `logs`' `--follow` |
| `inspect NAME\|ID...` | containers, then images |
| `kill CONTAINER...` | |
| `exec [-d] [-i] [-t] [-e K=V] [-w DIR] [-u USER] CONTAINER CMD...` | a further process in a running container (needs `--wasi engine`): same files, network and capabilities, in its process group (counted against its limits, killed with it); exits with the command's status. `-t` gives it a terminal: the engine does the line editing and echo (`tty_set` switches them), output gets CR LF; `-u` accepts only `root`/`0` (`POST /containers/{id}/exec`, `POST /exec/{id}/start`, `POST /exec/{id}/resize`, `GET /exec/{id}/json`) |
| `start CONTAINER...` | starts stopped containers (they keep their files and logs); `-a/--attach` (one container) prints this run's output and exits with its status |
| `stop CONTAINER...`, `restart CONTAINER...` | `-t/--time` accepted and ignored: a wasm guest has no signals, so stopping is killing, at once |
| `diff CONTAINER` | the files it added (`A`), changed (`C`) or deleted (`D`) |
| `events` | `--since`, `--until` (Unix seconds, RFC 3339, or a duration such as `10m`), `-f/--filter` `type=`, `event=`, `container=`, `image=`, `volume=`, `network=`, `label=`; streams container `create start die kill stop restart destroy prune`, image `pull untag delete prune` and network `create connect disconnect destroy prune` (`GET /events`; the last 1024 events are kept for `--since`) |
| `stats [CONTAINER...]` | `--no-stream`, `-a/--all`; linear memory against its limit, bytes written, pids (`GET /containers/{id}/stats`, one reading or one per second). CPU is shown as `--`: a WebAssembly guest gives the engine no CPU accounting, so none is invented |
| `volume create\|ls\|inspect\|rm\|prune` | named volumes: `create [--label K=V] [NAME]`, `ls [-q]`, `rm [-f]` (refused while any container mounts it), `prune [-a] [-f]` (anonymous ones; `-a` every unused one) |
| `network create\|ls\|inspect\|rm\|connect\|disconnect\|prune` | `create [--label K=V] NAME` (an in-engine network, the next free `10.89+n.0.0/16`), `ls [-q]`, `inspect` (with the running containers' addresses), `rm` (refused while a container uses it; `bridge`, `none` and a class network cannot be removed), `connect [--alias NAME] NETWORK CONTAINER` (a running container at once), `disconnect [-f] NETWORK CONTAINER`, `prune [-f] [--filter label=K[=V]\|label!=K=V]` |
| `version` | |

**For builders** (API only for now): `POST /images/load` takes an OCI image
layout as a tar (each `index.json` entry named by `io.containerd.image.name`
or `org.opencontainers.image.ref.name`, else `<none>`; checked as a pull is,
nothing read from the host); `PUT /containers/{id}/archive?path=DIR` extracts
a tar into a container (relative clean names only, no devices, hard links
inside, paths resolved inside the container, its quota applies;
`noOverwriteDirNonDir=1`), `GET` returns a tar of a path, and `HEAD` only
its `X-Docker-Container-Path-Stat` (no tar is built; `api.Client.StatPath`). Those two
uploads are bounded by `server.Info.MaxUploadBytes` (1 GiB by default),
every other request at 1 MiB. `GET /images/{name}/json` shows the full config,
the history, and `RootFS.Layers` as diff_ids. Other modules add commands to
`wctr` through `cli.Options.Extra` (each receives the engine `-H` names).

**Volumes.** `run -v NAME:/path[:ro]` mounts a named volume (created on
first use); two containers mounting it share one tree, and it outlives
them, across restarts and reloads too. Host paths are refused: a
WebAssembly engine has no host filesystem to share. A volume is a WASI
preopen at its path — the way `wasmtime --dir host::guest` mounts — served
by both executors (natively by wazero's WASI or the engine's, in a browser
by the engine's): a guest resolves an absolute path against the longest
matching preopen, writes through `:ro` fail with EROFS, and a rename
across trees fails (EXDEV from the engine's WASI, as across mounts on
Linux; ENOSYS from wazero's, which does not handle it yet). Each volume is bounded (64 MiB, 16384 entries by default) and
stored like a container's writable layer.
| `completion bash\|zsh\|fish\|powershell` | prints a shell completion script |
| `system df` | `-v/--verbose`; images, containers' writable layers, and the state store (kind, bytes, quota) |
| `system prune` | `-a/--all` (every image no container uses), `-f/--force` (no question) |

**Containers survive a restart.** `wctrd` keeps every container — its
configuration, state, exit code, bounded logs and writable layer — under
`--data-root`/`containers`; the playground keeps them in IndexedDB. A
container that was running when the engine stopped comes back exited with
status 137; a `--rm` one is removed, as it would have been. Each container
is a small sealed record (sha256-checked) naming content-addressed blobs for
its logs and its layer (an OCI-style tar changeset), written blobs first, so
an interrupted write leaves the previous state; a record or blob that fails
its check is dropped and reported at start (`wctrd: repaired: …`).

**Restart policies** (`run --restart`, `HostConfig.RestartPolicy`), as
Docker's: `always` restarts a container whenever it exits and starts it with
the engine; `unless-stopped` the same, except that the engine leaves it
stopped if a user stopped it; `on-failure[:N]` restarts it when it exits
with a code other than 0, at most N times (0: no bound). A user's `stop` or
`kill` ends the policy until the next `start`, which counts from zero. Each
restart waits first — 100 ms, doubled after every run shorter than 10 s, at
most a minute — and meanwhile the container is `restarting` (listed by
`ps`, `Restarting (code) … ago`; `rm` needs `-f`). A start the engine refuses
(a volume, a network, a port) is not retried. `inspect` shows
`HostConfig.RestartPolicy`, `RestartCount` and `State.Restarting`.

**Shell completion.** `source <(wctr completion bash)` (or `zsh`, `fish`,
`powershell`; see `wctr completion bash --help` to install it for good).
Tab then completes subcommands and flags, image names for `run`, `pull`,
`rmi`, `inspect`, container names (or IDs from a prefix) for `logs`, `rm`,
`inspect`, running containers for `kill`, and example sizes for
`-m/--memory`, with descriptions in zsh and fish. The candidates come from
the daemon, through cobra's hidden `__complete` command — the same code the
playground's terminal calls in-process (`cli.Complete`).

**Line editor for embedders.** `cli/lineedit` is wctr's interactive line
editor as a pure model (no terminal, DOM or widget type): `New(Options)`,
`Key(ctx, Key) Event` (editing keys, history Up/Down and Ctrl-P/N, Ctrl-A/E/
B/F/U/K/W/L/C/D, reverse search Ctrl-R, Tab completion — unique match,
longest common prefix, second Tab lists, "Display all N possibilities?"
past 100), `State()` (prompt, line, cursor), `History`/`SetHistory`;
`WctrCompleter(cli.Options, builtins)` completes through `__complete`;
`ParseKeys` reads terminal input and `Renderer` writes ANSI for a terminal,
both optional. The playground's terminal and the dashboard's terminal
widget drive the same model.

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
implemented. `POST /exec/{id}/start` does not hijack the connection as
Docker does: it answers an ordinary streamed response (multiplexed, or raw
with a terminal), and reads stdin from the request body after the
`ExecStartConfig` object (see `api/exec.go`), which also works over the
playground's in-process transport.

## Networks

```console
$ wctr network create lab
$ wctr run -d --name web --network lab netprobe serve 80
$ wctr run --rm --network lab netprobe get http://web/class
hello class from 10.89.0.3:49153
```

A guest opens sockets with the **WasmEdge socket extension** of
`wasi_snapshot_preview1` (`sock_open`, `sock_bind`, `sock_listen`,
`sock_connect`, `sock_recv_from`, `sock_send_to`, `sock_getaddrinfo`, …, in
its V2 form, as [stealthrocket/wasi-go](https://github.com/stealthrocket/wasi-go)
serves it), plus preview 1's `sock_accept`, `sock_recv`, `sock_send`,
`sock_shutdown`: a stock Go program using
[stealthrocket/net](https://github.com/stealthrocket/net)'s `wasip1`
package (`Listen`, `Dial`, `net/http` over them) runs unchanged. The engine's
WASI host serves them (natively, `wctrd --wasi engine`, the default; in a
browser, always); it never blocks inside a call — a waiting socket
operation, or a `poll_oneoff` on sockets, hands the executor a channel to
wait on, so the browser's engine worker keeps its event loop.

- **Every network has DNS by container name.** A guest's stub resolver
  (Go's, musl's) asks `127.0.0.1:53` when the image has no
  `/etc/resolv.conf` (most wasm images), or `127.0.0.11:53`, Docker's
  address: the engine answers there, from the network — A and AAAA for
  names on it, NXDOMAIN otherwise, nothing forwarded outside, answers within
  512 bytes (`golang.org/x/net/dns/dnsmessage`, fuzzed). A guest built with
  stealthrocket/net's `getaddrinfo` tag resolves through `sock_getaddrinfo`
  instead; both are tested. Unlike Docker's default bridge, `bridge` has
  name resolution too.
- **In-engine networks** (`bridge`, `10.88.0.0/16`; `network create`):
  streams are in-memory and flow-controlled (64 KiB per direction: a
  container that stops reading slows its peer), datagrams are at most
  8 KiB and a full queue (256) drops; `127.0.0.1` is the container itself.
  A container is reachable by its name when that is a DNS label, else by
  its short ID.
- **Aliases and several networks.** `--network-alias` (or
  `NetworkingConfig.EndpointsConfig[net].Aliases` at create, or
  `network connect --alias`) adds names on a user-defined network; an alias
  several containers share answers every one of them, in an order that
  rotates from query to query (a compose service's replicas). A container
  joins more networks at create (`EndpointsConfig`, API 1.44+) or with
  `network connect`, a running one at once: its listeners open on the new
  network too, a dial goes out through the network holding the address,
  and names resolve on all of them. `network disconnect` takes it off one;
  off its last, it has no network. Connect, disconnect, create and destroy
  are `network` events.
- **Filters.** `GET /containers/json` (`label`, `name`, `id`, `status`,
  `network`), `GET /networks` (`label`, `name`, `id`, `driver`),
  `GET /volumes` (`label`, `name`, `dangling`) and `POST /networks/prune`
  (`label`) honour Docker's filters (`k`, `k=v`, `k!=v` for labels; both
  JSON forms); any other filter is refused with 400 rather than ignored.
  In Go: `api.Client`'s `ContainersFiltered`, `NetworksFiltered` and
  `VolumesFiltered` take an `api.Filters`; `Containers`, `Networks` and
  `Volumes` keep their signatures, which other modules' interfaces embed.
- **A class network**: `wctrd --relay https://relay:8443
  --relay-join-code-file FILE [--relay-ca ca.pem]` joins a class of a
  [relay](https://github.com/go-wasmcontainers/relay) and adds a network
  (`class` by default, `--relay-network`) on which containers get the
  class's addresses and names (`web`, `web.alice`, `web.alice.<domain>`),
  under the teacher's firewall. The join code is read from a file (never on
  the command line, refused if others can read it); the session token is
  kept `0600` in the data root, refreshed before it expires, and reused at
  the next start. The contract is the relay's `docs/NETWORKDRIVER.md`. A
  host may also add and remove such a network at run time
  (`network.Manager.AddDriver` / `RemoveDriver`): the playground's
  "Join class" does.
- `--network none` (and `--wasi wazero`) gives no sockets: `sock_open`
  fails with `ENOTSUP`. `--network host` is refused.

### Published ports

```
$ wctr run -d --name web -p 8080:80 netprobe serve 80
$ curl http://127.0.0.1:8080/hi        # from the host, into the container
$ wctr port web
80/tcp -> 127.0.0.1:8080
```

A container has no socket on the host: its listeners live on the engine's
networks. A published port is a listener **wctrd** opens on the host
(package `portmap`), and each connection it accepts is dialed into the
container through the container's own endpoint, at `127.0.0.1:PORT` — the
port is reached on whichever of its networks the guest listens. Bytes are
copied both ways, half-closes included (Docker's userland proxy, not
iptables); UDP keeps one session per client, ended after 90 s of silence.

- **⛔ A binding without an address listens on `127.0.0.1`, not on every
  address as Docker does.** `-p 8080:80` makes a lab's container reachable
  from this machine only; `-p 0.0.0.0:8080:80` says it should be reachable
  from the network.
- The Engine API shapes are Docker's: `HostConfig.PortBindings`,
  `HostConfig.PublishAllPorts`, `Config.ExposedPorts`, inspect's
  `NetworkSettings.Ports` (`null` for a port only exposed), the summary's
  `Ports`. A host port range takes the first free port in it; a host port
  of `""` or `0` is one the system picks, reported as picked.
- A host port somebody holds refuses the start, as Docker words it:
  `Bind for 127.0.0.1:8080 failed: port is already allocated`. Nothing of
  that container is left listening. Ports are opened at start and freed
  when the container stops, so a restart may pick other ones under `-P`.
- A container on `--network none` cannot publish (refused at start, where
  Docker would publish a port that never answers), nor can an engine with no
  host to listen on (`container.Options.Ports` nil).
- A start refused before the process exists (a port, a network, a volume)
  is recorded as an exit with code 128 and the reason, as Docker records
  it: a `run --rm` returns at once and its container is removed.
- In a browser the host is the page, not a machine: the playground will
  provide a `portmap.Host` behind a Service Worker route
  (`/playground/port/<n>/…`).

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

## Processes (WASIX subset)

With the engine's WASI host (`wctrd --wasi engine`, the default), a guest
can start other programs of its container through a **subset of
[WASIX](https://wasix.org/)**, in WASIX's own import module `wasix_32v1`
and ABI (wasix-libc's `api_wasix.h`): `proc_spawn2`, `proc_spawn3`,
`proc_join`, `proc_id`, `proc_parent`, `fd_pipe`, `tty_get`, `tty_set` —
no fork, exec, threads or signals. A child is a new instance of a module
of the same root filesystem, with a copy of its parent's descriptors
changed by the spawn's file actions (close, dup2, open, chdir, fchdir);
`proc_join` waits without blocking the engine; pipes hold 64 KiB and wake
`poll_oneoff`. A container's processes form one group: at most 64 at once,
32 spawns per second (bursts of 64), killed together with the container.
This is what runs `sh` and its pipelines in the
[go-wasmcontainers/shell](https://github.com/go-wasmcontainers/shell) base
image.

## Packages

| Package | What it does |
|---|---|
| `image` | OCI content store (content-addressed, every blob sha256-verified) over a pluggable backend (directory, memory, or the playground's IndexedDB), manifests, indexes, configs; pull from an OCI image layout or a registry's `/v2` API with token auth, or from a mirror with a path prefix (`RegistryOptions.Mirrors`) |
| `rootfs` | in-memory root filesystem: layers applied per the OCI layer spec (whiteouts, opaque whiteouts), frozen as a shared read-only lower, cloned copy-on-write per container |
| `runtime` | the `Executor` interface (no WebAssembly runtime type in it) |
| `runtime/wazero` | the native executor: wazero, WASI preview 1 (wazero's, or the engine's own with `EngineWASI`), memory limit, wall-clock timeout, compiled modules cached per sha256 |
| `runtime/browser` | the browser executor (`GOOS=js`): one Web Worker per container process, its WASI calls served synchronously by the engine's worker over `SharedArrayBuffer` + `Atomics.wait`, guest memory capped and shared by rewriting the module's memory limits; `worker.js` is embedded |
| `wasi` | the engine's WASI preview 1 host: every import Go's `wasip1` port uses, over a byte-addressed memory and the container's `rootfs` — one implementation for the native and the browser executors |
| `wasmbin` | rewrites a module's memory limits (maximum, shared) |
| `netdriver` | the `NetworkDriver` / `Endpoint` contract, `Local` (an in-engine network, `x/net/nettest`-conformant streams), `WithDNS` (the network's DNS server) |
| `netdriver/relaynet` | a class network through the relay: join, connect, token refresh |
| `network` | the named networks (`bridge`, `none`, created ones, host-provided drivers), kept in the state store |
| `container` | lifecycle `created` → `running` → `exited` (restartable; `restarting` under a restart policy), exit codes, bounded logs, wait conditions, names, ID prefixes; checkpoints to and restores from a `state.Store` |
| `state` | the key-value `Store` the engine persists containers in (directory, memory; IndexedDB in the playground), sealed records, content-addressed blobs |
| `daemon` | image store + unpacked image trees + container manager; the create override rules |
| `api`, `api/server`, `api/inproc` | the API: types, client, multiplexed stream; the HTTP server; an in-memory transport for an in-process client (the browser has no sockets) |
| `cli` | the `wctr` command tree, importable (the playground runs it in-process) |
| `cmd/wctr`, `cmd/wctrd` | the CLI (cobra; also builds for `GOOS=wasip1`) and the daemon. `cmd/wctr` is a **module of its own** (`github.com/go-wasmcontainers/engine/cmd/wctr`, tags `cmd/wctr/vX.Y.Z`): it adds `wctr build` ([build](https://github.com/go-wasmcontainers/build)) and `wctr compose` ([compose](https://github.com/go-wasmcontainers/compose)), which depend on the engine and so cannot be imported from its module. Install the two with two `go install` commands: one command naming both no longer works, as they are in different modules. Engine tags up to v0.14.0 still hold `cmd/wctr` (without build and compose) |

## In a browser

The [playground](https://github.com/go-wasmcontainers/playground) runs this
engine in a web page: the daemon, its API and the `wctr` command tree in one
Web Worker (`GOOS=js`), each container in its own worker. A container's
WASI calls are answered by package `wasi` in the engine's worker, against
the same copy-on-write `rootfs` as natively, through a `SharedArrayBuffer`
control block and `Atomics.wait`; the engine reads and writes the guest's
memory directly, because `wasmbin` makes it shared (and caps it, which is
how the memory limit is enforced there). The same `wasi` package is run
natively by every guest test (`runtime/wazero`, `EngineWASI`), so the
browser path's file-system semantics are tested without a browser.

The browser executor can keep compiled modules across page loads
(`Options.CodeCache`: Cache Storage plus `WebAssembly.compileStreaming`,
the only path V8's code cache serves; a hit is checked byte for byte
against the module derived from the verified image). Measured in Chromium
on a 2.8 MB Go module, it does not pay: the compile call takes 8.4 ms from
Cache Storage against 1.5 ms from bytes (V8 compiles lazily), and a
CPU-bound guest after a reload runs in the same time either way (111–113
ms against 111.5 ms). The playground therefore leaves it off by default,
and its e2e keeps measuring both (`?codecache=1`).

## Performance

Measured on an Apple M4 Max, one core (`-cpu 1`), go1.27.3; benchmarks are
committed (`go test -run '^$' -bench . -cpu 1 ./image ./rootfs ./runtime/wazero`):

| Benchmark | What | Median of 3 |
|---|---|---|
| `BenchmarkPullLayout` | cold pull of the `hello` image (735 kB tar+gzip layer) from an OCI layout: verify + store | 1.26 ms (583 MB/s) |
| `BenchmarkUnpackGzipLayer` | apply that layer (2.4 MB module) to an empty tree | 11.5 ms (210 MB/s, bound by `compress/gzip`) |
| `BenchmarkUnpackZstdLayer` | the same layer as `tar+zstd` (default level), bounded single-threaded decoder | 2.1 ms against 8.3 ms for gzip in the same run (load 32: indicative) |
| `BenchmarkClone` | a container's copy-on-write root plus its first write | 2.2 µs |
| `BenchmarkStartCold` | first container of an image: wazero compiles the 2.4 MB Go module, runs it to exit | 610 ms |
| `BenchmarkStartWarm` | every later container: compiled module from the per-digest cache | 3.4 ms |
| `BenchmarkWriteDiff` | one checkpoint of a writable layer holding 8 MiB in 64 files (then sha256 of the blob, ≈4 ms) | 0.25 ms (load 17: indicative) |
| `BenchmarkApplyDiff` | restoring that layer at start | 0.47 ms (load 17: indicative) |

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
  length, depth and gzip or zstd ratio (200:1) are bounded, as is a zstd
  frame's window (64 MiB, refused before it is allocated); devices and FIFOs are
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
- **Stored state** is read back as untrusted: records are sealed with their
  kind and sha256, blobs are checked against their content-addressed keys, a
  writable layer goes through the layer checks under the container's quota,
  and the browser's compiled-module cache is hashed before a module from it
  runs.
- **Network**: a container reaches only its network (no host network, no
  outside DNS); every socket buffer in the host is bounded (64 KiB each way
  per socket, 256 datagrams, 64 accepted connections, 8 KiB datagrams);
  every guest pointer and length in the socket calls is bounds-checked;
  a container's sockets close when it exits. The relay's join code and
  session token never appear in a URL, a command line or the environment.
- **Processes**: the WASIX calls bound what they read from the guest (a
  4 KiB name, 256 file actions, 16384 strings, 256 KiB of argv and env
  together), check every guest pointer, resolve programs only inside the
  container's root, and are limited per container (process count, memory
  reserved across instances, spawn rate).
- **Published ports** listen on `127.0.0.1` unless a binding names an
  address; each binding proxies at most 128 connections and 256 UDP
  clients at once (8 KiB datagrams, larger ones dropped); at most 1024
  bindings per container; the `-p` and PortBindings parsers are fuzzed.
- **Fuzzed** in CI (corpus committed under `testdata/fuzz`): the WASIX
  process calls, the `-p` and PortBindings parsers, the socket
  calls (sequences of them on two processes of one network), the DNS
  answerer, the layer
  unpacker and the writable-layer reader, the path resolver, the document
  parsers, the reference parser, the client's demultiplexer, and the stored
  record, log and seal decoders. `govulncheck` runs on every change.

## Limitations

- No stdin attach and no terminal for a container's first process (`run
  -it`); `exec -it` has both. No `exec` in the browser executor yet.
  Process spawning is the WASIX subset above (no signals: a child is
  stopped only with its container; ^C in a terminal erases the line).
- Published ports: no sctp; no browser host yet (the playground's Service
  Worker route comes next); the source address the guest sees is its own,
  not the client's.
- Networks: a container on several networks
  binds to its primary address or to all (`0.0.0.0`), not to a secondary
  address alone; aliases only on in-engine networks; IPv4 only on in-engine
  networks, no `AF_UNIX`; the DNS
  answers on the resolver addresses shadow a container's own UDP server on
  port 53 there; an unconnected UDP `sendto` to the resolver is not
  answered (stub resolvers connect).
- No `build`, compose, or Kubernetes API yet; volumes are local and named
  only (no drivers, no host bind mounts, no tmpfs).
- A running container's writes are stored every 2 s and at exit: a crash
  loses at most the last 2 s of them. Hard links to an unchanged image file
  come back as copies.
- Layers: `tar`, `tar+gzip`, `tar+zstd` (OCI, and pkgx bottles'
  `application/vnd.pkgx.bottle.layer.v1.tar+zstd`); hard links from image layers are
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
