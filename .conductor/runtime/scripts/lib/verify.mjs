import fs from "node:fs";
import path from "node:path";
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import { readState } from "./state.mjs";
import { validateMap, resolveOwnership } from "../../extension/conductor-ownership.mjs";

const MAX_OUTPUT = 128 * 1024;
function boundedAppend(current, chunk) { const next = current + chunk.toString("utf8"); return next.length > MAX_OUTPUT ? next.slice(0, MAX_OUTPUT) : next; }

export function runCommand(argv, { cwd = ".", timeoutMs = 300000 } = {}) {
  if (!Array.isArray(argv) || !argv.length || argv.some((x) => typeof x !== "string")) return Promise.reject(new Error("argv must be a non-empty string array"));
  return new Promise((resolve) => {
    const started = Date.now(); let stdout = ""; let stderr = ""; let timedOut = false; let settled = false;
    const timers = []; const schedule = (ms, fn) => { const timer = setTimeout(fn, ms); timer.unref(); timers.push(timer); };
    const child = spawn(argv[0], argv.slice(1), { cwd, shell: false, env: process.env, stdio: ["ignore", "pipe", "pipe"], detached: process.platform !== "win32" });
    const clearTimers = () => { for (const timer of timers.splice(0)) clearTimeout(timer); };
    const finish = (result) => { if (settled) return; settled = true; clearTimers(); resolve({ durationMs: Date.now() - started, stdout, stderr, truncated: stdout.length >= MAX_OUTPUT || stderr.length >= MAX_OUTPUT, ...result }); };
    const killTree = (signal) => { let killed = false; if (child.pid && process.platform !== "win32") { try { process.kill(-child.pid, signal); killed = true; } catch {} } if (!killed) { try { child.kill(signal); } catch {} } };
    child.stdout.on("data", (chunk) => { stdout = boundedAppend(stdout, chunk); });
    child.stderr.on("data", (chunk) => { stderr = boundedAppend(stderr, chunk); });
    child.on("error", (error) => finish({ status: "failed", exitCode: null, signal: null, timedOut, stderr: `${stderr}${error.message}` }));
    child.on("close", (code, signal) => finish({ status: code === 0 && !timedOut ? "passed" : "failed", exitCode: code, signal, timedOut }));
    schedule(timeoutMs, () => {
      timedOut = true; killTree("SIGTERM");
      schedule(200, () => { if (!settled) killTree("SIGKILL"); });
      // Hard cap: inherited descriptors or a broken close event cannot extend the
      // declared timeout by more than a short, fixed cleanup window.
      schedule(500, () => { if (!settled) finish({ status: "failed", exitCode: null, signal: "SIGKILL", timedOut, stderr: `${stderr}\ncleanup bound exceeded; process tree killed` }); });
    });
  });
}

function gitChangedFiles(root, baseline) {
  return new Promise(async (resolve) => {
    const diff = await runCommand(["git", "diff", "--name-only", "--no-ext-diff", baseline, "--"], { cwd: root, timeoutMs: 10000 });
    const untracked = await runCommand(["git", "ls-files", "--others", "--exclude-standard"], { cwd: root, timeoutMs: 10000 });
    if (diff.status !== "passed" || untracked.status !== "passed") return resolve({ error: `${diff.stderr}\n${untracked.stderr}`.trim() });
    resolve({ files: [...new Set(`${diff.stdout}\n${untracked.stdout}`.split("\n").filter(Boolean))].sort() });
  });
}

export function validateVerificationPlan(plan) {
  if (!plan || plan.schemaVersion !== 1 || !Array.isArray(plan.checks)) throw new Error("invalid verification plan");
  const fail = (message) => { throw new Error(`invalid verification plan: ${message}`); };
  const text = (v) => typeof v === "string" && v.length > 0 && !v.includes("\0");
  const list = (v) => Array.isArray(v) && v.every(text) && new Set(v).size === v.length;
  if (plan.maxConcurrency !== undefined && (!Number.isSafeInteger(plan.maxConcurrency) || plan.maxConcurrency < 1 || plan.maxConcurrency > 32)) fail("maxConcurrency must be 1..32");
  const ids = new Map();
  for (const c of plan.checks) {
    if (!c || !text(c.id) || ids.has(c.id)) fail("missing or duplicate check id");
    ids.set(c.id, c);
    if (!["command", "state-schema", "map-schema", "file-exists", "diff-scope"].includes(c.kind)) fail(`unknown kind for ${c.id}`);
    for (const key of ["required", "parallelSafe"]) if (c[key] !== undefined && typeof c[key] !== "boolean") fail(`${key} must be boolean`);
    if (c.timeoutMs !== undefined && (!Number.isSafeInteger(c.timeoutMs) || c.timeoutMs < 1 || c.timeoutMs > 2147483647)) fail("timeoutMs must be 1..2147483647");
    for (const key of ["dependsOn", "resources"]) if (c[key] !== undefined && !list(c[key])) fail(`${key} must contain unique non-empty strings`);
    for (const key of ["cwd", "path", "baselineCommit"]) if (c[key] !== undefined && !text(c[key])) fail(`invalid ${key}`);
    if (c.kind === "command" && (!Array.isArray(c.argv) || !c.argv.length || !text(c.argv[0]) || c.argv.some((v) => typeof v !== "string" || v.includes("\0")))) fail(`invalid argv for ${c.id}`);
    if (c.kind === "file-exists" && (!list(c.paths) || !c.paths.length)) fail("paths must be a non-empty string array");
    if (c.kind === "diff-scope" && (!list(c.allowedPaths) || !text(c.baselineCommit || plan.baselineCommit))) fail("diff-scope requires allowedPaths and baselineCommit");
  }
  const visiting = new Set(); const visited = new Set();
  const visit = (id) => {
    if (!ids.has(id)) fail(`unknown dependency ${id}`);
    if (visiting.has(id)) fail(`dependency cycle at ${id}`);
    if (visited.has(id)) return;
    visiting.add(id); for (const dep of ids.get(id).dependsOn || []) visit(dep);
    visiting.delete(id); visited.add(id);
  };
  for (const id of ids.keys()) visit(id);
}

export async function runVerificationPlan(plan, { root = "." } = {}) {
  validateVerificationPlan(plan);
  const results = new Array(plan.checks.length);
  const execute = async (check) => {
    const started = Date.now(); let result;
    try {
      if (check.kind === "command") result = await runCommand(check.argv, { cwd: path.resolve(root, check.cwd || "."), timeoutMs: check.timeoutMs });
      else if (check.kind === "state-schema") { readState(path.resolve(root, check.path || ".conductor-state.md")); result = { status: "passed" }; }
      else if (check.kind === "map-schema") { const map = JSON.parse(fs.readFileSync(path.resolve(root, check.path || ".conductor/section-map.json"), "utf8")); const validation = validateMap(map); result = { status: validation.ok ? "passed" : "failed", stderr: validation.errors.join("\n") }; }
      else if (check.kind === "file-exists") { const missing = (check.paths || []).filter((item) => !fs.existsSync(path.resolve(root, item))); result = { status: missing.length ? "failed" : "passed", stderr: missing.length ? `missing: ${missing.join(", ")}` : "" }; }
      else if (check.kind === "diff-scope") {
        const changed = await gitChangedFiles(root, check.baselineCommit || plan.baselineCommit);
        if (changed.error) result = { status: "failed", stderr: changed.error };
        else { const denied = changed.files.filter((file) => !(check.allowedPaths || []).some((prefix) => file === prefix || file.startsWith(`${prefix.replace(/\/$/, "")}/`))); result = { status: denied.length ? "failed" : "passed", changedFiles: changed.files, stderr: denied.length ? `out-of-scope: ${denied.join(", ")}` : "" }; }
      } else throw new Error(`unknown verification check kind '${check.kind}'`);
    } catch (error) { result = { status: "failed", stderr: error.message }; }
    return { id: check.id, kind: check.kind, required: check.required !== false, durationMs: result.durationMs ?? Date.now() - started, ...result };
  };
  const pending = new Set(plan.checks.map((_, i) => i)); const active = new Map();
  const indices = new Map(plan.checks.map((c, i) => [c.id, i]));
  const exclusive = (c) => c.kind === "command" && c.parallelSafe !== true;
  while (pending.size || active.size) {
    for (const i of pending) {
      const c = plan.checks[i]; const deps = (c.dependsOn || []).map((id) => results[indices.get(id)]);
      if (deps.some((r) => !r)) continue;
      const blockedBy = deps.filter((r) => r.status !== "passed").map((r) => r.id);
      if (blockedBy.length) {
        results[i] = { id: c.id, kind: c.kind, required: c.required !== false, durationMs: 0, status: "skipped", blockedBy, stderr: `blocked by: ${blockedBy.join(", ")}` };
        pending.delete(i); continue;
      }
      if (active.size >= (plan.maxConcurrency ?? 3)) continue;
      if ([...active.keys()].some((j) => exclusive(c) || exclusive(plan.checks[j]) || (c.resources || []).some((r) => (plan.checks[j].resources || []).includes(r)))) continue;
      pending.delete(i);
      active.set(i, execute(c).then((result) => { results[i] = result; active.delete(i); }));
    }
    if (active.size) await Promise.race(active.values());
  }
  const status = results.every((item) => !item.required || item.status === "passed") ? "passed" : "failed";
  return { schemaVersion: 1, runId: plan.runId, at: new Date().toISOString(), status, checks: results };
}

export function validateReviewVerdict(verdict) {
  const errors = [];
  if (!verdict || verdict.schemaVersion !== 1) return ["review schemaVersion must be 1"];
  if (!["accepted", "rejected", "infra-failure"].includes(verdict.verdict)) errors.push("invalid review verdict");
  if (typeof verdict.reviewedSnapshot !== "string" || !verdict.reviewedSnapshot) errors.push("reviewedSnapshot is required");
  if (!Array.isArray(verdict.findings) || !Array.isArray(verdict.relaunchSections) || !Array.isArray(verdict.residualRisks)) errors.push("review arrays are required");
  for (const finding of verdict.findings || []) if (!finding.severity || !finding.section || !finding.message) errors.push("each finding requires severity, section, and message");
  if (verdict.verdict === "accepted" && verdict.findings?.some((x) => ["blocker", "high"].includes(x.severity))) errors.push("accepted verdict cannot contain blocker/high findings");
  return errors;
}

export function consumeReview(current, verdict) {
  const errors = validateReviewVerdict(verdict); if (errors.length) throw new Error(errors.join("; "));
  if (current?.expectedSnapshot !== undefined && current.expectedSnapshot !== verdict.reviewedSnapshot) throw new Error("review snapshot does not match expectedSnapshot");
  const priorAttempts = current?.attempts ?? 0;
  // Old records did not distinguish infrastructure failures: retain their conservative budget.
  const priorInfra = current?.infraFailures ?? priorAttempts;
  if (![priorAttempts, priorInfra].every((n) => Number.isSafeInteger(n) && n >= 0) || priorInfra > priorAttempts) throw new Error("invalid review counters");
  const attempts = priorAttempts + 1;
  const infraFailures = priorInfra + (verdict.verdict === "infra-failure" ? 1 : 0);
  const base = { attempts, infraFailures, lastSnapshot: verdict.reviewedSnapshot };
  if (verdict.verdict === "accepted") return { ...base, status: "accepted", verdict: "accepted" };
  if (verdict.verdict === "rejected") return { ...base, status: "rejected", verdict: "rejected", relaunchSections: verdict.relaunchSections };
  if (infraFailures >= 2) return { ...base, status: "infra-closed", verdict: "needs-human-review" };
  return { ...base, status: "infra-failure", verdict: "pending" };
}

export function snapshotDigest(files) {
  const hash = crypto.createHash("sha256");
  for (const file of [...files].sort()) { hash.update(file); hash.update("\0"); if (fs.existsSync(file)) hash.update(fs.readFileSync(file)); hash.update("\0"); }
  return hash.digest("hex");
}
