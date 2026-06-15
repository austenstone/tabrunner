# tabrunner

**The first WebAssembly GitHub Actions runner. Turn any browser tab into a self-hosted runner.** 🏃‍♂️💨

Open a page, paste a registration token, and your browser tab becomes a live GitHub Actions runner that GitHub can dispatch jobs to. No install, no OS, no `./config.sh`. One tab, one runner. Open ten tabs, get ten runners.

> ⚠️ **Experimental.** This is a research project chasing a hard goal: run real, unmodified marketplace actions (yes, including [`actions/checkout`](https://github.com/actions/checkout)) inside a WebAssembly sandbox. See [ROADMAP.md](./ROADMAP.md) for what works and what doesn't yet.

## Why

Self-hosted runners today need a real OS, a real process, and a multi-step install. tabrunner asks a different question: what if the runner is **portable down to the instruction set**? If steps execute inside a Wasm runtime, a runner can live anywhere Wasm lives, most provocatively, an unmodified browser tab.

The pitch in one line: **bring your own tab.** Host it publicly, send someone a link, and they contribute compute by leaving a tab open.

## The hard part (and why it's worth it)

Running a `run:` shell step in Wasm is easy. Running `actions/checkout` unmodified is not, because checkout is a `node20` JavaScript action that shells out to real `git`. Making that work means assembling a **tiny POSIX-flavored OS in Wasm**:

1. A **shared virtual filesystem** that persists across steps and across the JS engine + the git module.
2. A **process model** where `child_process.spawn('git', ...)` resolves to a `git` Wasm module in that same FS.
3. A **Node-compatible JS runtime** on Wasm to execute `dist/index.js` with the `@actions/toolkit` surface.
4. **Networking** for HTTPS git fetches (native via [WASIX](https://wasix.org) sockets, relayed in-browser).

The good news: [WASIX](https://wasix.org) already provides most of that kernel (fork/exec, sockets, shared FS) and ships `bash`/`coreutils`/`git` as runnable Wasm. We're assembling it for CI, not building it from scratch. Full detail in [ARCHITECTURE.md](./ARCHITECTURE.md).

## Status

| Tier | What | State |
|------|------|-------|
| 1 | `run:` shell steps (bash/coreutils/git in Wasm) | 🟡 in progress (M0) |
| 2 | JavaScript actions (`actions/checkout`) | ⚪ planned (M2-M3) |
| 3 | Docker actions | 🔴 out of scope |

Full milestone ladder in [ROADMAP.md](./ROADMAP.md).

## Stack

- **Core**: Go ([wazero](https://github.com/tetratelabs/wazero) for the sandbox, no cgo)
- **Execution substrate**: [WASIX](https://wasix.org) (`bash`, `coreutils`, `git` as Wasm)
- **Job acquisition**: [actions/scaleset](https://github.com/actions/scaleset)
- **References**: [nektos/act](https://github.com/nektos/act) (execution model), [github-act-runner](https://github.com/ChristopherHX/github-act-runner) (protocol + telemetry)

## License

[MIT](./LICENSE) © Austen Stone
