// Dynamic imports, not import declarations: the CI typescript action wraps
// this file in an async function body, where top-level `import` is a syntax
// error (TS1232). Dynamic import works there and under plain `node` alike,
// and shadowing the action's injected `path`/`child_process` globals keeps
// the file self-contained for local runs.
const { createServer } = await import("node:net");
const { createHmac } = await import("node:crypto");
// Explicitly annotated: tsc requires assertion-function call targets
// (assert.ok and friends) to have a declared type (TS2775).
const assertNs = await import("node:assert/strict");
const assert: typeof assertNs.default = assertNs.default;
const path = await import("node:path");
const child_process = await import("node:child_process");

const BINARY = path.join("build", "webhook-runner");
const HOOKS_DIR = path.join("e2e", "hooks");

function freePort(): Promise<number> {
  return new Promise((resolve) => {
    const srv = createServer();
    srv.listen(0, "127.0.0.1", () => {
      const port = (srv.address() as { port: number }).port;
      srv.close(() => resolve(port));
    });
  });
}

async function waitForHealth(base: string, timeout = 15_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`${base}/health`);
      if (r.ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error("server did not become ready");
}

async function pollRun(base: string, runId: string, timeout = 60_000): Promise<any> {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const r = await fetch(`${base}/runs/${runId}`);
    const body: any = await r.json();
    if (body.status !== "pending" && body.status !== "running") return body;
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`run ${runId} did not complete`);
}

let passed = 0;
let failed = 0;

async function test(name: string, fn: () => Promise<void>) {
  try {
    await fn();
    console.log(`  PASS: ${name}`);
    passed++;
  } catch (e: any) {
    console.error(`  FAIL: ${name}: ${e.message}`);
    failed++;
  }
}

child_process.execSync("docker pull alpine:latest", { stdio: "inherit" });

const port = await freePort();
const adminPort = await freePort();
const base = `http://127.0.0.1:${port}`;
const adminBase = `http://127.0.0.1:${adminPort}`;
// hostenv-hook's hook.json references ${E2E_HOST_VAR}; the server process
// (spawned below, inheriting this env) expands it at container start.
process.env.E2E_HOST_VAR = "host-says-hi";
// sops-hook's secrets.sops.env decrypts with the committed test-only age key.
process.env.SOPS_AGE_KEY_FILE = path.resolve("e2e", "age-test-key.txt");
const proc = child_process.spawn(
  BINARY,
  ["--addr", `:${port}`, "--admin-addr", `:${adminPort}`, HOOKS_DIR],
  { stdio: ["ignore", "inherit", "inherit"] },
);

try {
  await waitForHealth(base);

  await test("GET /health returns 200", async () => {
    const r = await fetch(`${base}/health`);
    assert.equal(r.status, 200);
    const body: any = await r.json();
    assert.equal(body.status, "ok");
  });

  await test("GET /hooks lists all hooks (admin port)", async () => {
    const r = await fetch(`${adminBase}/hooks`);
    assert.equal(r.status, 200);
    const hooks: any = await r.json();
    assert.equal(hooks.length, 10);
    const ids = hooks.map((h: any) => h.id).sort();
    assert.deepEqual(ids, ["apikey-hook", "echo-test", "env-hook", "fail-hook", "hookdir-hook", "hostenv-hook", "mount-hook", "secure-hook", "sleep-hook", "sops-hook"]);
  });

  await test("GET /hooks not on hook port", async () => {
    const r = await fetch(`${base}/hooks`);
    assert.equal(r.status, 404);
  });

  await test("sync trigger returns output and payload", async () => {
    const r = await fetch(`${base}/hook/echo-test?wait=true`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ sender: "e2e-test" }),
    });
    assert.equal(r.status, 200);
    const run: any = await r.json();
    assert.equal(run.status, "success");
    assert.equal(run.exit_code, 0);
    assert.equal(run.hook_id, "echo-test");
    const output = run.output.join("\n");
    assert.ok(output.includes("hello-e2e"), "missing hello-e2e");
    assert.ok(output.includes("e2e-test"), "missing payload content");
  });

  await test("GET /runs/:id returns the run (admin port)", async () => {
    const trigger = await fetch(`${base}/hook/echo-test?wait=true`, { method: "POST", body: "{}" });
    const run: any = await trigger.json();
    const r = await fetch(`${adminBase}/runs/${run.id}`);
    assert.equal(r.status, 200);
    const fetched: any = await r.json();
    assert.equal(fetched.id, run.id);
    assert.equal(fetched.status, "success");
  });

  await test("async trigger returns 202 with run_id", async () => {
    const r = await fetch(`${base}/hook/echo-test`, { method: "POST", body: "{}" });
    assert.equal(r.status, 202);
    const body: any = await r.json();
    assert.ok(body.run_id, "missing run_id");
    const result = await pollRun(adminBase, body.run_id);
    assert.equal(result.status, "success");
  });

  await test("POST /hook/nonexistent returns 404", async () => {
    const r = await fetch(`${base}/hook/nonexistent`, { method: "POST", body: "{}" });
    assert.equal(r.status, 404);
  });

  await test("container failure propagates exit code", async () => {
    const r = await fetch(`${base}/hook/fail-hook?wait=true`, { method: "POST", body: "{}" });
    assert.equal(r.status, 500);
    const run: any = await r.json();
    assert.equal(run.status, "failure");
    assert.equal(run.exit_code, 42);
    assert.ok(run.output.join("\n").includes("failing"));
  });

  await test("API key: valid key accepted", async () => {
    const r = await fetch(`${base}/hook/apikey-hook?wait=true`, {
      method: "POST",
      headers: { "X-API-Key": "e2e-test-key-42" },
      body: "{}",
    });
    assert.equal(r.status, 200);
  });

  await test("API key: wrong key rejected", async () => {
    const r = await fetch(`${base}/hook/apikey-hook`, {
      method: "POST",
      headers: { "X-API-Key": "wrong" },
      body: "{}",
    });
    assert.equal(r.status, 401);
  });

  await test("API key: missing key rejected", async () => {
    const r = await fetch(`${base}/hook/apikey-hook`, { method: "POST", body: "{}" });
    assert.equal(r.status, 401);
  });

  await test("legacy HMAC: bad signature rejected", async () => {
    const r = await fetch(`${base}/hook/secure-hook`, {
      method: "POST",
      headers: { "X-Hub-Signature-256": "sha256=deadbeef" },
      body: '{"x":1}',
    });
    assert.equal(r.status, 401);
  });

  await test("legacy HMAC: valid signature accepted", async () => {
    const payload = '{"secure":"payload"}';
    const sig = createHmac("sha256", "e2e-secret-key").update(payload).digest("hex");
    const r = await fetch(`${base}/hook/secure-hook?wait=true`, {
      method: "POST",
      headers: { "X-Hub-Signature-256": `sha256=${sig}`, "Content-Type": "application/json" },
      body: payload,
    });
    assert.equal(r.status, 200);
  });

  await test("env vars are injected", async () => {
    const r = await fetch(`${base}/hook/env-hook?wait=true`, { method: "POST", body: "{}" });
    assert.equal(r.status, 200);
    const run: any = await r.json();
    const output = run.output.join("\n");
    assert.ok(output.includes("id=env-hook"), "missing HOOK_ID");
    assert.ok(output.includes("custom=e2e-value"), "missing MY_VAR");
  });

  await test("payload and headers are mounted", async () => {
    const r = await fetch(`${base}/hook/mount-hook?wait=true`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: '{"mounted":"yes"}',
    });
    assert.equal(r.status, 200);
    const sync: any = await r.json();
    const full: any = await (await fetch(`${adminBase}/runs/${sync.id}`)).json();
    const output = full.output.join("\n");
    assert.ok(output.includes('"mounted":"yes"'), "missing payload");
    assert.ok(output.includes("Content-Type"), "missing headers");
  });

  await test("HOOK_DIR: the hook's own folder is mounted", async () => {
    const r = await fetch(`${base}/hook/hookdir-hook?wait=true`, { method: "POST", body: "{}" });
    assert.equal(r.status, 200);
    const run: any = await r.json();
    assert.ok(run.output.join("\n").includes("hello-from-hook-dir"), "missing hook-dir file content");
  });

  await test("env ${VAR} expands from the runner host", async () => {
    const r = await fetch(`${base}/hook/hostenv-hook?wait=true`, { method: "POST", body: "{}" });
    assert.equal(r.status, 200);
    const run: any = await r.json();
    assert.ok(run.output.join("\n").includes("fromhost=host-says-hi"), "missing expanded host env value");
  });

  await test("sops secrets: injected env, ${NAME} reference, and api_key all decrypt", async () => {
    // SOPS_HOOK_KEY lives only inside the encrypted secrets.sops.env.
    const r = await fetch(`${base}/hook/sops-hook?wait=true`, {
      method: "POST",
      headers: { "X-API-Key": "sops-sesame-77" },
      body: "{}",
    });
    assert.equal(r.status, 200);
    const run: any = await r.json();
    const output = run.output.join("\n");
    assert.ok(output.includes("msg=hello-from-sops"), "missing injected secret");
    assert.ok(output.includes("ref=sops-ref-value"), "missing ${NAME}-referenced secret");
  });

  await test("sops secrets: wrong api_key rejected", async () => {
    const r = await fetch(`${base}/hook/sops-hook`, {
      method: "POST",
      headers: { "X-API-Key": "wrong" },
      body: "{}",
    });
    assert.equal(r.status, 401);
  });

  await test("cancel kills an in-flight run", async () => {
    const trigger = await fetch(`${base}/hook/sleep-hook`, { method: "POST", body: "{}" });
    assert.equal(trigger.status, 202);
    const { run_id } = (await trigger.json()) as any;
    // Wait until the container has demonstrably started (its first echo
    // arrived), so docker kill has a real container to hit.
    const deadline = Date.now() + 30_000;
    for (;;) {
      const s: any = await (await fetch(`${adminBase}/runs/${run_id}`)).json();
      if ((s.output ?? []).join("\n").includes("sleeping")) break;
      if (Date.now() > deadline) throw new Error(`run never produced output (status ${s.status})`);
      await new Promise((r) => setTimeout(r, 200));
    }
    const c = await fetch(`${base}/hook/sleep-hook/cancel/${run_id}`, { method: "POST" });
    assert.equal(c.status, 202);
    const result = await pollRun(adminBase, run_id);
    assert.equal(result.status, "cancelled");
    assert.equal(result.cancel_requested, true);
  });

  await test("cancel of a finished run returns 409", async () => {
    const trigger = await fetch(`${base}/hook/echo-test?wait=true`, { method: "POST", body: "{}" });
    const run: any = await trigger.json();
    const c = await fetch(`${base}/hook/echo-test/cancel/${run.id}`, { method: "POST" });
    assert.equal(c.status, 409);
  });

  await test("cancel with a wrong hook id returns 404", async () => {
    const trigger = await fetch(`${base}/hook/echo-test?wait=true`, { method: "POST", body: "{}" });
    const run: any = await trigger.json();
    const c = await fetch(`${base}/hook/env-hook/cancel/${run.id}`, { method: "POST" });
    assert.equal(c.status, 404);
  });

  await test("GET /runs/nonexistent returns 404 (admin port)", async () => {
    const r = await fetch(`${adminBase}/runs/nonexistent`);
    assert.equal(r.status, 404);
  });

  await test("GET /runs lists runs (admin port)", async () => {
    const r = await fetch(`${adminBase}/runs`);
    assert.equal(r.status, 200);
    const runs: any = await r.json();
    assert.ok(runs.length >= 1, "should have at least 1 run");
  });

  await test("GET /runs?hook= filters by hook (admin port)", async () => {
    const r = await fetch(`${adminBase}/runs?hook=echo-test`);
    assert.equal(r.status, 200);
    const runs: any = await r.json();
    assert.ok(runs.length >= 2, `echo-test should have >= 2 runs, got ${runs.length}`);
  });
} finally {
  proc.kill();
}

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
