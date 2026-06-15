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
};

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
