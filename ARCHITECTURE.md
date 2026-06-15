# Architecture

## The runner is two halves

1. **Protocol + telemetry** — acquire a job, stream timeline records + live logs + the final conclusion back to GitHub. Pure HTTP/JSON logic, portable, Wasm-safe. [github-act-runner](https://github.com/ChristopherHX/github-act-runner) is the reference implementation; [actions/scaleset](https://github.com/actions/scaleset) gives us clean, supported job acquisition.
2. **Execution** — actually run the steps. The only half WebAssembly constrains.

Most of the orchestration layer (job parsing, step sequencing, the `${{ }}` expression engine, contexts, the file-command protocol, workflow commands, masking, matrices, `if:`/`continue-on-error:`/`post:`) is portable logic we reimplement once. The interesting work is execution.

## Execution is a tiny OS in Wasm

To run an unmodified marketplace action like [`actions/checkout`](https://github.com/actions/checkout), we need four things working together. checkout is a `node20` action whose `dist/index.js` shells out to real `git` via `@actions/exec`, so:

### 1. Shared virtual filesystem
A single WASI filesystem instance shared across every module in a job: the JS engine, the `git` module, and successive steps. checkout writes the repo, the next `run:` step reads it. Backed by wazero's `fs.FS` mounts natively, or an in-memory FS in the browser.

### 2. Process model
A registry mapping command names to Wasm modules (`git`, `bash`, `coreutils`, the JS runtime). `child_process.spawn('git', args)` becomes "instantiate the git module with the shared FS + env + args + stdio pipes, return exit code + stdout/stderr." This is a mini process table over a shared FS. [WASIX](https://wasix.org) already provides fork/exec primitives, which is why it's the most credible base.

### 3. Node-compatible JS runtime
The make-or-break piece. To run `dist/index.js` we need a JS engine on Wasm that provides the `@actions/toolkit` surface: `fs`, `os`, `path`, `process`, `child_process`, `https`, `crypto`, `stream`. Candidates:

- **QuickJS-on-Wasm** (Javy/quickjs-emscripten): runs the JS, zero Node APIs. We'd build the Node-compat shim over our own FS + process + networking.
- **[WinterJS](https://github.com/wasmerio/winterjs)** (Wasmer, SpiderMonkey-based): web/service-worker APIs, partial Node compat.
- **WASIX `node`** (if it exists with real `fs`/`child_process`/`net`): the dream path, collapses the whole problem.

The toolkit surface is large but finite. The two genuinely hard dependencies are `child_process` (means "run another program", solved by #2) and `https`/`net` (solved by #4).

### 4. Networking
checkout fetches over HTTPS (the token-authed tarball / refs) and `git` clones over HTTPS.

- **Native host** (wazero/WASIX server-side): sockets work via the host. Fine.
- **Browser host**: no raw TCP/TLS. Every socket is proxied through a WebSocket → TCP relay, with TLS terminated at the relay or run in-Wasm. Same relay the CORS-blocked job-acquisition traffic needs.

## Why this assembles instead of being built from scratch

[WASIX](https://wasix.org) = WASI + threads + fork/exec + sockets + signals, and Wasmer ships `bash`, `coreutils`, and `git` as Wasm packages that already interoperate over a shared FS with subprocess support. So #1, #2, and the Tier-1 `run:` toolbox largely exist. The remaining frontier is #3 (the Node-compat JS runtime) and, for the browser, #4 (the relay).

## Tiers, restated as execution difficulty

- **Tier 1 (`run:`):** WASIX bash/coreutils/git. Tractable now.
- **Tier 2 (JS actions):** #1 + #2 + #3 + #4. The real build. checkout is the hero.
- **Tier 3 (Docker actions):** needs an OCI/container runtime in Wasm ([container2wasm](https://github.com/ktock/container2wasm) territory). Out of scope.

## Native-first, browser-eventually

We prove the model server-side with [wazero](https://github.com/tetratelabs/wazero) (pure Go, no cgo, no CORS) through M4, then port the core to `wasm32` and add the relay for M5. The orchestration logic stays identical; only the execution host and the networking shim change.
