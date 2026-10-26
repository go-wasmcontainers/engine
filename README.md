# engine

[![Docs](https://img.shields.io/badge/docs-hugo%20%2B%20hextra-0079A8)](https://go-wasmcontainers.github.io/docs/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-wasmcontainers/engine/actions/workflows/ci.yml/badge.svg)](https://github.com/go-wasmcontainers/engine/actions/workflows/ci.yml)
![status](https://img.shields.io/badge/status-M1%20in%20progress-yellow)

**A Docker-compatible container engine whose containers are WASI preview 1
(`wasip1`) WebAssembly modules**, in pure Go (`CGO_ENABLED=0`). It is the
engine of the [go-wasmcontainers](https://go-wasmcontainers.github.io/)
training labs, which will run entirely in a web browser (see
[playground](https://github.com/go-wasmcontainers/playground)). Milestone
M1 runs natively on Linux and macOS, with [wazero](https://wazero.io) as the
executor, so the whole core is testable in CI.

> **Status: M1 in progress.** Landed: the OCI content side (pulling images
> from an OCI image layout or an OCI distribution registry into a verified
> content store, unpacking layers into the container root filesystem), the
> executor (wazero, WASI preview 1) and the container lifecycle. The Docker
> API subset and the `docker` / `dockerd` commands follow.

## Packages

| Package | What it does |
|---|---|
| `image` | OCI content store (content-addressed, every blob sha256-verified), manifests, indexes, configs; pull from an OCI image layout (`oci:<dir>[:<ref.name>]`) or a registry's `/v2` API with Docker token auth; platform selection `wasip1/wasm` |
| `rootfs` | in-memory root filesystem: layers applied per the OCI layer spec (whiteouts, opaque whiteouts), frozen as a shared read-only lower, cloned copy-on-write per container |
| `runtime` | the `Executor` interface (no WebAssembly runtime type in it, so a browser executor can implement it too) |
| `runtime/wazero` | the native executor: wazero, WASI preview 1, the container's root mounted writable as the guest's `/`, `PWD` set to the working directory, linear memory bounded (256 MiB default), wall-clock timeout, compiled modules cached per sha256 |
| `container` | lifecycle `created` → `running` → `exited` (restartable), exit codes, bounded logs (stdout/stderr records), wait conditions `not-running` / `next-exit` / `removed`, names, ID prefixes |

## Images it runs

Both shapes, selected from an index by platform `wasip1/wasm`:

- the CNCF TAG-Runtime **wasm OCI artifact**: config
  `application/vnd.wasm.config.v0+json`, one `application/wasm` layer;
- an **ordinary OCI image** whose `tar` / `tar+gzip` layers hold a
  `.wasm` module (e.g. `/bin/app.wasm`) named by the config's `Entrypoint`.

## Security

Registry and layer content is treated as hostile:

- every manifest, index, config and blob is bounded in size and verified
  against its digest before use; a registry's `Docker-Content-Digest` must
  match the content; `Authorization` never follows a redirect to another host;
- layer unpacking refuses absolute names, `..`, writing under a symlink or a
  file, symlink targets that climb above the root, hard links to anything but
  an existing regular file; it bounds total bytes, entries, path length and
  depth, and the gzip decompression ratio (200:1 by default);
- the root filesystem is entirely in memory: an absolute symlink is relative
  to the container's root, so nothing a layer or a container does reaches
  the host;
- the layer unpacker, the path resolver, the document parsers and the
  reference parser are fuzzed in CI (corpus committed under `testdata/fuzz`).

## References

Every format and protocol is implemented from its specification; the list,
with what each one decides, is in [docs/REFERENCES.md](docs/REFERENCES.md).

## License

BSD 3-Clause, see [LICENSE](LICENSE).
