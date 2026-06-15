// app.js wires the page to the Go/Wasm runner.
//
// Flow: load tabrunner.wasm via wasm_exec.js -> the Go main() registers a global
// tabrunnerRun(yaml) and calls tabrunnerReady() -> we enable the Run button.
// Clicking Run hands the editor contents to tabrunnerRun and renders the output
// (translating ANSI color codes to HTML so it looks like real CI logs).

const editor = document.getElementById("editor");
const consoleEl = document.getElementById("console");
const runBtn = document.getElementById("run");
const statusEl = document.getElementById("status");
const examplesEl = document.getElementById("examples");

const ghurlEl = document.getElementById("ghurl");
const regtokenEl = document.getElementById("regtoken");
const labelsEl = document.getElementById("labels");
const configcmdEl = document.getElementById("configcmd");
const newRunnerLink = document.getElementById("newrunner-link");
const connectBtn = document.getElementById("connect");
const connectStatusEl = document.getElementById("connect-status");
const connectStatusText = document.getElementById("connect-status-text");
const connectDot = connectStatusEl ? connectStatusEl.querySelector(".dot") : null;
const connectLog = document.getElementById("connect-log");

const EXAMPLES = {
  hello: {
    label: "Hello tabrunner",
    yaml: `name: Hello tabrunner
on: push

jobs:
  greet:
    name: Greet
    runs-on: self-hosted
    steps:
      - name: Say hello
        run: echo "Hello from inside Wasm!"

      - name: Produce an output
        id: gen
        run: |
          echo "greeting=hi-from-wasm" >> $GITHUB_OUTPUT
          echo "Wrote an output"

      - name: Pass data via GITHUB_ENV
        run: echo "SHARED=passed-through" >> $GITHUB_ENV

      - name: Consume previous output and env
        run: |
          echo "output was: \${{ steps.gen.outputs.greeting }}"
          echo "env was: $SHARED"
          printf "multi\\nline\\n" > note.txt
          cat note.txt
`,
  },
  files: {
    label: "Filesystem & redirection",
    yaml: `name: Files
on: push

jobs:
  fs:
    name: Play with the in-memory FS
    runs-on: self-hosted
    steps:
      - name: Write some files
        run: |
          mkdir -p data
          echo "line one" > data/log.txt
          echo "line two" >> data/log.txt
          printf "alpha\\nbeta\\ngamma\\n" > data/list.txt

      - name: Read them back
        run: |
          echo "--- log.txt ---"
          cat data/log.txt
          echo "--- list.txt ---"
          cat data/list.txt

      - name: List the directory
        run: ls data
`,
  },
  matrix: {
    label: "Multiple steps & env",
    yaml: `name: Steps and env
on: push

jobs:
  build:
    name: Fake build
    runs-on: self-hosted
    steps:
      - name: Set version
        id: ver
        run: echo "version=1.4.2" >> $GITHUB_OUTPUT

      - name: Export build dir
        run: echo "BUILD_DIR=dist" >> $GITHUB_ENV

      - name: Compile
        run: |
          mkdir -p $BUILD_DIR
          echo "tabrunner \${{ steps.ver.outputs.version }}" > $BUILD_DIR/VERSION
          echo "compiled into $BUILD_DIR"

      - name: Show artifact
        run: cat $BUILD_DIR/VERSION
`,
  },
};

// Populate the example dropdown and prefill the editor.
for (const [key, ex] of Object.entries(EXAMPLES)) {
  const opt = document.createElement("option");
  opt.value = key;
  opt.textContent = ex.label;
  examplesEl.appendChild(opt);
}
editor.value = EXAMPLES.hello.yaml;
examplesEl.addEventListener("change", () => {
  const ex = EXAMPLES[examplesEl.value];
  if (ex) editor.value = ex.yaml;
});

// Minimal ANSI SGR -> HTML. Handles the codes the runner actually emits.
function ansiToHtml(text) {
  const esc = (s) =>
    s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  let html = "";
  let open = 0;
  const re = /\x1b\[([0-9;]*)m/g;
  let last = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    html += esc(text.slice(last, m.index));
    last = re.lastIndex;
    const codes = m[1].split(";").filter((c) => c !== "");
    for (const code of codes) {
      if (code === "0" || code === "") {
        while (open-- > 0) html += "</span>";
        open = 0;
      } else {
        html += `<span class="ansi-${code}">`;
        open++;
      }
    }
  }
  html += esc(text.slice(last));
  while (open-- > 0) html += "</span>";
  return html;
}

function setStatus(msg, kind) {
  statusEl.textContent = msg;
  statusEl.className = "status" + (kind ? " " + kind : "");
}

let ready = false;

// Go calls this when the runtime is initialized.
globalThis.tabrunnerReady = function () {
  ready = true;
  runBtn.disabled = false;
  runBtn.textContent = "▶ Run";
  consoleEl.innerHTML =
    '<span class="dim">Ready. Edit the workflow and hit Run.</span>';
  setStatus("");

  if (connectBtn) {
    connectBtn.disabled = false;
    connectBtn.textContent = "Connect";
  }
  if (connectStatusText) connectStatusText.textContent = "Idle — ready to connect";
  if (connectDot) connectDot.className = "dot";
};

let connecting = false;

function connectStatus(stage, detail) {
  if (!connectStatusEl) return;
  const dotClass = { listening: "live", registering: "work", job: "work", error: "err", disconnected: "" };
  if (connectDot) connectDot.className = "dot" + (dotClass[stage] ? " " + dotClass[stage] : "");
  const labels = {
    registering: "Registering runner…",
    listening: "Listening for jobs",
    job: "Running job",
    error: "Error",
    disconnected: "Disconnected",
  };
  if (connectStatusText) connectStatusText.textContent = (labels[stage] || stage) + (detail ? " — " + detail : "");

  if (connectLog) {
    connectLog.hidden = false;
    const line = document.createElement("div");
    line.textContent = "[" + stage + "] " + (detail || "");
    connectLog.appendChild(line);
    connectLog.scrollTop = connectLog.scrollHeight;
  }

  if (stage === "disconnected" || stage === "error") {
    connecting = false;
    if (connectBtn) {
      connectBtn.disabled = false;
      connectBtn.textContent = "Connect";
    }
  }
}

// Go calls this to report connection lifecycle progress.
globalThis.tabrunnerStatus = connectStatus;

if (connectBtn) {
  connectBtn.addEventListener("click", () => {
    if (!ready || connecting) return;
    const url = (ghurlEl.value || "").trim();
    const token = (regtokenEl.value || "").trim();
    const labels = (labelsEl.value || "").trim();
    if (!url || !token) {
      connectStatus("error", "GitHub URL and registration token are required");
      return;
    }

    connecting = true;
    connectBtn.disabled = true;
    connectBtn.textContent = "Connecting…";
    connectStatus("registering", url);

    const res = globalThis.tabrunnerConnect(url, token, labels);
    const err = res && res.error;
    if (err) connectStatus("error", err);
  });
}

// Parse a `./config.sh --url ... --token ... [--labels ...]` command string.
function parseConfigCommand(text) {
  return {
    url: ((text.match(/--url\s+(\S+)/) || [])[1] || "").trim(),
    token: ((text.match(/--token\s+(\S+)/) || [])[1] || "").trim(),
    labels: ((text.match(/--labels\s+(\S+)/) || [])[1] || "").trim(),
  };
}

// Derive the "add runner" settings URL from a GitHub org or repo URL.
function newRunnerUrl(ghurl) {
  const m = (ghurl || "").trim().match(/github\.com\/([^/\s]+)(?:\/([^/\s]+))?/i);
  if (!m) return null;
  const owner = m[1];
  const repo = m[2];
  return repo
    ? `https://github.com/${owner}/${repo}/settings/actions/runners/new`
    : `https://github.com/organizations/${owner}/settings/actions/runners/new`;
}

function syncNewRunnerLink() {
  if (!newRunnerLink) return;
  const u = newRunnerUrl(ghurlEl && ghurlEl.value);
  if (u) newRunnerLink.href = u;
}

function applyConfigCommand(text) {
  const { url, token, labels } = parseConfigCommand(text);
  if (url && ghurlEl) ghurlEl.value = url;
  if (token && regtokenEl) regtokenEl.value = token;
  if (labels && labelsEl) labelsEl.value = labels;
  syncNewRunnerLink();
}

if (configcmdEl) {
  configcmdEl.addEventListener("input", () => applyConfigCommand(configcmdEl.value));
}

// Also let users paste the full config.sh command straight into the URL/token fields.
[ghurlEl, regtokenEl].forEach((el) => {
  if (!el) return;
  el.addEventListener("paste", (e) => {
    const clip = e.clipboardData || window.clipboardData;
    const t = clip ? clip.getData("text") : "";
    if (t && /--url\s+\S+|--token\s+\S+/.test(t)) {
      e.preventDefault();
      applyConfigCommand(t);
    }
  });
});

if (ghurlEl) ghurlEl.addEventListener("input", syncNewRunnerLink);
syncNewRunnerLink();

runBtn.addEventListener("click", () => {
  if (!ready) return;
  runBtn.disabled = true;
  runBtn.textContent = "Running…";
  setStatus("");
  consoleEl.innerHTML = '<span class="dim">Running…</span>';

  // Defer so the browser can paint the "Running…" state before the
  // synchronous Wasm work blocks the main thread.
  setTimeout(() => {
    const start = performance.now();
    let result;
    try {
      result = globalThis.tabrunnerRun(editor.value);
    } catch (e) {
      consoleEl.innerHTML = "";
      setStatus("crash: " + e, "err");
      runBtn.disabled = false;
      runBtn.textContent = "▶ Run";
      return;
    }
    const ms = Math.round(performance.now() - start);

    const output = (result && result.output) || "";
    const error = (result && result.error) || "";
    let html = ansiToHtml(output);
    if (error) {
      html +=
        '\n<span class="ansi-31">error: ' +
        error.replace(/&/g, "&amp;").replace(/</g, "&lt;") +
        "</span>";
    }
    consoleEl.innerHTML =
      html || '<span class="dim">(no output)</span>';
    setStatus(
      (error ? "failed" : "done") + ` in ${ms}ms`,
      error ? "err" : "ok"
    );
    runBtn.disabled = false;
    runBtn.textContent = "▶ Run";
  }, 20);
});

// Boot the Wasm module.
(async function boot() {
  if (!globalThis.Go) {
    setStatus("wasm_exec.js failed to load", "err");
    return;
  }
  const go = new Go();
  try {
    const resp = await fetch("tabrunner.wasm");
    const result = await WebAssembly.instantiateStreaming(resp, go.importObject);
    go.run(result.instance); // resolves only when Go exits; main blocks forever
  } catch (e) {
    consoleEl.innerHTML = "";
    setStatus("failed to load Wasm: " + e, "err");
  }
})();
