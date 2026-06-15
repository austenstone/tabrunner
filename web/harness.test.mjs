// Headless smoke test: load the js/wasm build under Node (same Go runtime as the
// browser) and call tabrunnerRun to prove a workflow actually executes.
import fs from "fs";
import path from "path";
import { fileURLToPath } from "url";

const dir = path.dirname(fileURLToPath(import.meta.url));
// wasm_exec.js sets globalThis.Go (it has Node detection built in).
await import(path.join(dir, "wasm_exec.js"));

const go = new globalThis.Go();
const bytes = fs.readFileSync(path.join(dir, "tabrunner.wasm"));
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);

const ready = new Promise((resolve) => {
  globalThis.tabrunnerReady = resolve;
  setTimeout(() => resolve("timeout"), 8000);
});

go.run(instance); // blocks in Go's select{}; exported func still callable
const r = await ready;
if (r === "timeout") {
  console.error("FAIL: runtime never signaled ready");
  process.exit(1);
}

const yaml = fs.readFileSync(path.join(dir, "..", "examples", "hello.yml"), "utf8");
const res = globalThis.tabrunnerRun(yaml);
console.log("=== output ===");
console.log(res.output);
if (res.error) {
  console.error("=== error ===\n" + res.error);
  process.exit(1);
}
if (!res.output.includes("Hello from inside Wasm!") || !res.output.includes("env was: passed-through")) {
  console.error("FAIL: expected output markers missing");
  process.exit(1);
}
console.log("PASS: workflow executed in js/wasm");
