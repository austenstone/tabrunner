# Roadmap

The goal: a GitHub Actions runner whose steps execute inside a WebAssembly runtime, portable enough to run in a browser tab. We get there by proving the model natively first (no CORS, fastest path to a green job), then porting the core to the browser.

## Decision gate: the JS runtime (do this first)

Everything past Tier 1 hinges on running `node20` JavaScript actions in Wasm. Two strategies:

- **A1 (build it)**: [WinterJS](https://github.com/wasmerio/winterjs) or QuickJS + a Node-compat shim, wiring `child_process` to our Wasm process spawner. Max fidelity, max effort.
- **A2 (borrow it)**: if a real `node` compiled to [WASIX](https://wasix.org) exists with working `fs`/`child_process`/`net`, a JS action becomes `wasmer run node -- dist/index.js` and checkout's git-exec resolves to WASIX `git` for free. Collapses M2 + M3 ~10x.

**Task #1 is verifying whether A2 is real.** If yes, we win cheap. If not, we're on A1.

## Tiers

| Tier | Scope | Verdict |
|------|-------|---------|
| 1 | `run:` shell steps via WASIX bash/coreutils/git | Tractable now |
| 2 | JavaScript actions (the `actions/checkout` class) | The real build |
| 3 | Docker actions (`runs.using: docker`) | Out of scope (needs a container runtime in Wasm) |

⚠️ Some actions are semantically OS-bound regardless of runtime. `setup-node` will *run* under Tier 2 but is pointless (no native Node to install). `actions/checkout` is the right hero target because "fetch repo into the FS" is meaningful in Wasm.

## Milestones

### M0 — Orchestrator + first `run:` step
Parse a local workflow YAML, build the `${{ }}` expression/context engine, implement the file-command protocol (`$GITHUB_ENV`, `$GITHUB_OUTPUT`, `$GITHUB_PATH`, `$GITHUB_STATE`), and execute a `run:` step inside [wazero](https://github.com/tetratelabs/wazero) + WASIX bash.
**Hero:** a 2-step job runs and streams logs.

### M1 — Shared FS + process model
A shared WASI filesystem persisted across steps, plus a `spawn(cmd, args)` → Wasm-module registry.
**Hero:** `run: git clone` works via wasmer/git over the shared FS.

### M2 — Node-compat JS runtime
Stand up the JS action runtime (A1 or A2 per the decision gate). Wire `INPUT_*`, env files, and `child_process` → spawn.
**Hero:** a trivial JS action runs.

### M3 — `actions/checkout` unmodified 🎯
Download the action, run its `node20` entry, resolve its `git` execs to wasm-git over the shared FS, network via host/relay.
**Hero:** repo checked out, the next `run:` step sees the files. Thesis proven.

### M4 — Real job acquisition + telemetry
Swap the local YAML for [actions/scaleset](https://github.com/actions/scaleset) job acquisition and stream timeline records, live logs, and final conclusion back to GitHub.
**Hero:** a real GitHub job runs on tabrunner and turns green.

### M5 — Browser delivery (per-tab runner)
Port the runner core to `wasm32`, add the WebSocket relay (CORS + raw sockets), ship the public page.
**Hero:** open a tab, paste a token, your tab runs a job. 🏃‍♂️

## Sequencing notes

- M4 only depends on M0 (protocol is independent of the JS work), so it can run in parallel with M1-M3.
- M5 depends on both M3 and M4: you need real jobs *and* JS actions before the browser demo is honest.
- Networking is free natively (WASIX sockets via the host). In-browser, every socket goes through the relay, which is the same CORS relay job acquisition needs.
