import { createServer } from "node:net";
import { createHmac } from "node:crypto";
import assert from "node:assert/strict";

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
const base = `http://127.0.0.1:${port}`;
const proc = child_process.spawn(BINARY, ["--addr", `:${port}`, HOOKS_DIR], {
  stdio: ["ignore", "inherit", "inherit"],
});

try {
  await waitForHealth(base);

  await test("GET /health returns 200", async () => {
    const r = await fetch(`${base}/health`);
    assert.equal(r.status, 200);
    const body: any = await r.json();
    assert.equal(body.status, "ok");
  });

  await test("GET /hooks lists all hooks", async () => {
    const r = await fetch(`${base}/hooks`);
    assert.equal(r.status, 200);
    const hooks: any = await r.json();
    assert.equal(hooks.length, 6);
    const ids = hooks.map((h: any) => h.id).sort();
    assert.deepEqual(ids, ["apikey-hook", "echo-test", "env-hook", "fail-hook", "mount-hook", "secure-hook"]);
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

  await test("GET /runs/:id returns the run", async () => {
    const trigger = await fetch(`${base}/hook/echo-test?wait=true`, { method: "POST", body: "{}" });
    const run: any = await trigger.json();
    const r = await fetch(`${base}/runs/${run.id}`);
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
    const result = await pollRun(base, body.run_id);
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
    const full: any = await (await fetch(`${base}/runs/${sync.id}`)).json();
    const output = full.output.join("\n");
    assert.ok(output.includes('"mounted":"yes"'), "missing payload");
    assert.ok(output.includes("Content-Type"), "missing headers");
  });

  await test("GET /runs/nonexistent returns 404", async () => {
    const r = await fetch(`${base}/runs/nonexistent`);
    assert.equal(r.status, 404);
  });

  await test("GET /runs lists runs", async () => {
    const r = await fetch(`${base}/runs`);
    assert.equal(r.status, 200);
    const runs: any = await r.json();
    assert.ok(runs.length >= 1, "should have at least 1 run");
  });

  await test("GET /runs?hook= filters by hook", async () => {
    const r = await fetch(`${base}/runs?hook=echo-test`);
    assert.equal(r.status, 200);
    const runs: any = await r.json();
    assert.ok(runs.length >= 2, `echo-test should have >= 2 runs, got ${runs.length}`);
  });
} finally {
  proc.kill();
}

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
