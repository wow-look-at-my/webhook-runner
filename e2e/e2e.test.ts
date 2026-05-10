import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { execSync, spawn, ChildProcess } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { createServer } from "node:net";
import { createHmac } from "node:crypto";
import { readdirSync } from "node:fs";
import { platform, arch } from "node:os";

const REPO_ROOT = join(import.meta.dirname, "..");

function findBinary(): string {
  const buildDir = join(REPO_ROOT, "build");
  const goarch = arch() === "x64" ? "amd64" : arch();
  const suffix = platform() === "win32" ? ".exe" : "";
  const platformBin = join(buildDir, `webhook-runner_${platform()}_${goarch}${suffix}`);
  const plainBin = join(buildDir, `webhook-runner${suffix}`);
  try {
    readdirSync(buildDir);
  } catch {
    throw new Error(`build directory not found: ${buildDir}`);
  }
  for (const candidate of [platformBin, plainBin]) {
    try {
      execSync(`test -x "${candidate}"`, { stdio: "ignore" });
      return candidate;
    } catch {}
  }
  const files = readdirSync(buildDir);
  throw new Error(`no binary found in ${buildDir}: ${files.join(", ")}`);
}

const BINARY = findBinary();

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

async function pollRun(base: string, runId: string, timeout = 60_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const r = await fetch(`${base}/runs/${runId}`);
    const body = await r.json();
    if (body.status !== "pending" && body.status !== "running") return body;
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`run ${runId} did not complete`);
}

describe("webhook-runner e2e", () => {
  let hooksDir: string;
  let port: number;
  let base: string;
  let proc: ChildProcess;

  before(async () => {
    execSync("docker info", { stdio: "ignore" });
    execSync("docker pull alpine:latest", { stdio: "inherit" });

    hooksDir = mkdtempSync(join(tmpdir(), "wh-e2e-"));

    const writeHook = (id: string, config: object) => {
      const dir = join(hooksDir, id);
      mkdirSync(dir, { recursive: true });
      writeFileSync(join(dir, "hook.json"), JSON.stringify(config));
    };

    writeHook("echo-test", {
      description: "e2e echo test",
      image: "alpine:latest",
      command: ["sh", "-c", "echo hello-e2e && cat $HOOK_PAYLOAD_FILE"],
      timeout: "60s",
    });

    writeHook("fail-hook", {
      image: "alpine:latest",
      command: ["sh", "-c", "echo failing && exit 42"],
      timeout: "60s",
    });

    writeHook("secure-hook", {
      image: "alpine:latest",
      command: ["echo", "secure-output"],
      secret: "e2e-secret-key",
      timeout: "60s",
    });

    writeHook("apikey-hook", {
      image: "alpine:latest",
      command: ["echo", "apikey-ok"],
      api_key: "e2e-test-key-42",
      timeout: "60s",
    });

    writeHook("env-hook", {
      image: "alpine:latest",
      command: ["sh", "-c", "echo id=$HOOK_ID custom=$MY_VAR"],
      env: { MY_VAR: "e2e-value" },
      timeout: "60s",
    });

    writeHook("mount-hook", {
      image: "alpine:latest",
      command: [
        "sh",
        "-c",
        "cat $HOOK_PAYLOAD_FILE && echo --- && cat $HOOK_HEADERS_FILE",
      ],
      timeout: "60s",
    });

    port = await freePort();
    base = `http://127.0.0.1:${port}`;

    proc = spawn(BINARY, ["--addr", `:${port}`, hooksDir], {
      stdio: ["ignore", "inherit", "inherit"],
    });

    await waitForHealth(base);
  });

  after(() => {
    proc?.kill();
    rmSync(hooksDir, { recursive: true, force: true });
  });

  it("GET /health returns 200", async () => {
    const r = await fetch(`${base}/health`);
    assert.equal(r.status, 200);
    const body = await r.json();
    assert.equal(body.status, "ok");
  });

  it("GET /hooks lists all hooks", async () => {
    const r = await fetch(`${base}/hooks`);
    assert.equal(r.status, 200);
    const hooks = await r.json();
    assert.equal(hooks.length, 6);
    const ids = hooks.map((h: { id: string }) => h.id).sort();
    assert.deepEqual(ids, [
      "apikey-hook",
      "echo-test",
      "env-hook",
      "fail-hook",
      "mount-hook",
      "secure-hook",
    ]);
  });

  it("sync trigger returns output and payload", async () => {
    const r = await fetch(`${base}/hook/echo-test?wait=true`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ sender: "e2e-test" }),
    });
    assert.equal(r.status, 200);
    const run = await r.json();
    assert.equal(run.status, "success");
    assert.equal(run.exit_code, 0);
    assert.equal(run.hook_id, "echo-test");
    const output = run.output.join("\n");
    assert.ok(output.includes("hello-e2e"), "missing hello-e2e");
    assert.ok(output.includes("e2e-test"), "missing payload content");
  });

  it("GET /runs/:id returns the run", async () => {
    const trigger = await fetch(`${base}/hook/echo-test?wait=true`, {
      method: "POST",
      body: "{}",
    });
    const run = await trigger.json();
    const r = await fetch(`${base}/runs/${run.id}`);
    assert.equal(r.status, 200);
    const fetched = await r.json();
    assert.equal(fetched.id, run.id);
    assert.equal(fetched.status, "success");
  });

  it("async trigger returns 202 with run_id", async () => {
    const r = await fetch(`${base}/hook/echo-test`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(r.status, 202);
    const body = await r.json();
    assert.ok(body.run_id, "missing run_id");
    const result = await pollRun(base, body.run_id);
    assert.equal(result.status, "success");
  });

  it("POST /hook/nonexistent returns 404", async () => {
    const r = await fetch(`${base}/hook/nonexistent`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(r.status, 404);
  });

  it("container failure propagates exit code", async () => {
    const r = await fetch(`${base}/hook/fail-hook?wait=true`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(r.status, 500);
    const run = await r.json();
    assert.equal(run.status, "failure");
    assert.equal(run.exit_code, 42);
    assert.ok(run.output.join("\n").includes("failing"));
  });

  it("API key: valid key accepted", async () => {
    const r = await fetch(`${base}/hook/apikey-hook?wait=true`, {
      method: "POST",
      headers: { "X-API-Key": "e2e-test-key-42" },
      body: "{}",
    });
    assert.equal(r.status, 200);
    const run = await r.json();
    assert.equal(run.status, "success");
  });

  it("API key: wrong key rejected", async () => {
    const r = await fetch(`${base}/hook/apikey-hook`, {
      method: "POST",
      headers: { "X-API-Key": "wrong" },
      body: "{}",
    });
    assert.equal(r.status, 401);
  });

  it("API key: missing key rejected", async () => {
    const r = await fetch(`${base}/hook/apikey-hook`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(r.status, 401);
  });

  it("legacy HMAC: bad signature rejected", async () => {
    const r = await fetch(`${base}/hook/secure-hook`, {
      method: "POST",
      headers: { "X-Hub-Signature-256": "sha256=deadbeef" },
      body: '{"x":1}',
    });
    assert.equal(r.status, 401);
  });

  it("legacy HMAC: valid signature accepted", async () => {
    const payload = '{"secure":"payload"}';
    const sig = createHmac("sha256", "e2e-secret-key")
      .update(payload)
      .digest("hex");
    const r = await fetch(`${base}/hook/secure-hook?wait=true`, {
      method: "POST",
      headers: {
        "X-Hub-Signature-256": `sha256=${sig}`,
        "Content-Type": "application/json",
      },
      body: payload,
    });
    assert.equal(r.status, 200);
    const run = await r.json();
    assert.equal(run.status, "success");
  });

  it("env vars are injected", async () => {
    const r = await fetch(`${base}/hook/env-hook?wait=true`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(r.status, 200);
    const run = await r.json();
    assert.equal(run.status, "success");
    const output = run.output.join("\n");
    assert.ok(output.includes("id=env-hook"), "missing HOOK_ID");
    assert.ok(output.includes("custom=e2e-value"), "missing MY_VAR");
  });

  it("payload and headers are mounted", async () => {
    const r = await fetch(`${base}/hook/mount-hook?wait=true`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: '{"mounted":"yes"}',
    });
    assert.equal(r.status, 200);
    const run = await r.json();
    assert.equal(run.status, "success");
    const output = run.output.join("\n");
    assert.ok(output.includes('"mounted":"yes"'), "missing payload");
    assert.ok(output.includes("Content-Type"), "missing headers");
  });

  it("GET /runs/nonexistent returns 404", async () => {
    const r = await fetch(`${base}/runs/nonexistent`);
    assert.equal(r.status, 404);
  });

  it("GET /runs lists runs", async () => {
    const r = await fetch(`${base}/runs`);
    assert.equal(r.status, 200);
    const runs = await r.json();
    assert.ok(runs.length >= 1, "should have at least 1 run");
  });

  it("GET /runs?hook= filters by hook", async () => {
    const r = await fetch(`${base}/runs?hook=echo-test`);
    assert.equal(r.status, 200);
    const runs = await r.json();
    assert.ok(runs.length >= 2, `echo-test should have >= 2 runs, got ${runs.length}`);
  });
});
