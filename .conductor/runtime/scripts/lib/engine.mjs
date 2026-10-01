// Conductor execution engine.
//
// Moves routine orchestration out of model turns and into deterministic code:
// dependency-ready scheduling (no whole-wave barrier), identity bound before the
// child prompt, runtime-owned liveness heartbeats, bounded budget handling,
// mechanical acceptance, and disk-based recovery. The model still owns contracts,
// scope, and substantive repair decisions; this engine never infers success from
// prose and never waives a failed check.
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import { readState, mutateState, atomicWrite, taskWaves, validateActiveAgreement } from "./state.mjs";
import { workOrderDigest, mapperDigest, resolveOwnership, validateMap } from "../../extension/conductor-ownership.mjs";
import { expectedReportPath, readJsonl, validateReportEvent } from "./report.mjs";
import { runCommand } from "./verify.mjs";
import { buildContextPacket } from "./context.mjs";
import { assertPlanAgreement, assertWriterEvidence, assertBuildSettled } from "./gates.mjs";
import { inspectRuntime, launchManagedChild, resolveInvocation, resolveSessionModel, mintRunId, mintToken, collectUsage } from "./rpc.mjs";

const TICK_MS = 250;
const sleep = (ms, deps) => (deps?.sleep ?? ((delay) => new Promise((resolve) => { const timer = setTimeout(resolve, delay); timer.unref?.(); }))(ms));
const now = (deps) => (deps?.now ?? Date.now)();

// Serialize state mutations: mutateState takes an exclusive lock and CAS, so two
// overlapping engine writes would fail rather than corrupt. Queueing keeps the
// engine's own concurrency from tripping that guard.
function createStateWriter(file, ownerToken) {
  let queue = Promise.resolve();
  return (options) => { const run = queue.then(() => mutateState(file, { ...options, ownerToken }), (error) => { throw error; }); queue = run.catch(() => null); return run; };
}

export function appendEvent(file, event) {
  if (!event || typeof event !== "object") throw new Error("event must be an object");
  const record = { at: event.at || new Date().toISOString(), ...event };
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const fd = fs.openSync(file, fs.constants.O_CREAT | fs.constants.O_WRONLY | fs.constants.O_APPEND, 0o600);
  try { fs.writeSync(fd, `${JSON.stringify(record)}\n`); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
  return record;
}

// Dependency-ready selection: a section starts as soon as ITS OWN prerequisites
// are accepted, not when an entire topological layer finishes.
export function readySections(state, { maxConcurrency = 3, inFlight = new Set() } = {}) {
  const sections = state.sections || {};
  const accepted = new Set(Object.entries(sections).filter(([, section]) => ["done", "skipped"].includes(section.status)).map(([id]) => id));
  const candidates = Object.entries(sections)
    .filter(([id, section]) => section.status === "pending" && !inFlight.has(id))
    .filter(([, section]) => (section.dependsOn || []).every((dep) => accepted.has(dep)))
    .map(([id]) => id).sort();
  const capacity = Math.max(0, maxConcurrency - inFlight.size);
  return { ready: candidates.slice(0, capacity), blocked: candidates.slice(capacity), pending: Object.values(sections).filter((s) => s.status === "pending").length };
}

export function allSettled(state) { return Object.values(state.sections || {}).every((section) => ["done", "skipped"].includes(section.status)); }

// Write or merge the snapshot-authorization marker. Each execution carries an
// immutable work-order digest plus a one-time capability token, so an unrelated
// state revision cannot authorize a stale or revoked child.
export function authorizeExecutions({ root, state, add = [], phase = "build" }) {
  const markerFile = path.join(root, ".conductor", "active.json");
  const existing = fs.existsSync(markerFile) ? JSON.parse(fs.readFileSync(markerFile, "utf8")) : null;
  if (existing && existing.authorization !== "snapshot") throw new Error("a legacy revision-bound active marker exists; settle or repair it before engine scheduling");
  if (existing && existing.runId !== state.run.id) throw new Error("active marker belongs to a different run");
  const kept = (existing?.executions || []).filter((item) => !add.some((entry) => entry.section === item.section));
  const executions = [...kept, ...add].sort((a, b) => a.section.localeCompare(b.section));
  if (!executions.length) throw new Error("authorizeExecutions requires at least one execution");
  const marker = { schemaVersion: 1, authorization: "snapshot", runId: state.run.id, stateRevision: state.revision, waveId: existing?.waveId || "engine", phase, executions };
  const errors = validateActiveAgreement(state, marker);
  if (errors.length) throw new Error(`authorization marker would be invalid: ${errors.join("; ")}`);
  atomicWrite(markerFile, `${JSON.stringify(marker, null, 2)}\n`);
  const reread = JSON.parse(fs.readFileSync(markerFile, "utf8"));
  const postErrors = validateActiveAgreement(state, reread);
  if (postErrors.length) throw new Error(`authorization marker postcondition failed: ${postErrors.join("; ")}`);
  return reread;
}

export function deauthorizeExecution({ root, state, sectionId }) {
  const markerFile = path.join(root, ".conductor", "active.json");
  if (!fs.existsSync(markerFile)) return { cleared: true, remaining: 0 };
  const marker = JSON.parse(fs.readFileSync(markerFile, "utf8"));
  const executions = (marker.executions || []).filter((item) => item.section !== sectionId);
  if (!executions.length) {
    if (marker.runId !== state.run.id) throw new Error("active marker belongs to a different run");
    fs.unlinkSync(markerFile);
    const dir = fs.openSync(path.dirname(markerFile), "r"); try { fs.fsyncSync(dir); } finally { fs.closeSync(dir); }
    if (fs.existsSync(markerFile)) throw new Error("active clear postcondition failed");
    return { cleared: true, remaining: 0 };
  }
  const next = { ...marker, stateRevision: state.revision, executions };
  const errors = validateActiveAgreement(state, next);
  if (errors.length) throw new Error(`deauthorization would leave an invalid marker: ${errors.join("; ")}`);
  atomicWrite(markerFile, `${JSON.stringify(next, null, 2)}\n`);
  return { cleared: false, remaining: executions.length };
}

// Mechanical acceptance. A writer is done only when its identity-bound report ends
// in a completed final with no remaining work, has no unresolved deviation or
// blocker, and every required declared command passes when the ENGINE runs it.
export async function evaluateAcceptance({ root, state, sectionId, piRunId, attempt }) {
  const section = state.sections[sectionId];
  const reportPath = expectedReportPath(root, state.run.id, section.agent, piRunId);
  const relative = path.relative(path.resolve(root), reportPath).split(path.sep).join("/");
  const failures = []; const checks = [];
  let events = [];
  try { events = readJsonl(reportPath); } catch (error) { failures.push(`report unreadable: ${error.message}`); }
  if (!events.length) failures.push("final report is missing");
  for (const [index, event] of events.entries()) {
    const errors = validateReportEvent(event);
    if (errors.length) failures.push(`report event ${index + 1} invalid: ${errors.join("; ")}`);
    if (event.seq !== index + 1) failures.push(`report event ${index + 1} has non-monotonic seq`);
  }
  const final = events.findLast((event) => event.kind === "final");
  if (!final) failures.push("report has no final event");
  else {
    if (final.outcome !== "completed") failures.push(`final outcome is '${final.outcome}', not completed`);
    if ((final.remaining || []).length) failures.push(`final event reports remaining work: ${final.remaining.length} unit(s)`);
    const map = JSON.parse(fs.readFileSync(path.resolve(root, state.map.path), "utf8"));
    for (const changed of final.changedFiles || []) {
      const resolution = resolveOwnership(map, changed);
      if (resolution.kind !== "owned" || resolution.owner !== sectionId) failures.push(`changed file '${changed}' is not owned by '${sectionId}'`);
      if (!(section.workOrder.declaredFiles || []).includes(changed)) failures.push(`changed file '${changed}' was not declared`);
    }
  }
  const resolved = new Set(section.resolvedEvents || []);
  for (const event of events) {
    const key = `${piRunId}:${event.seq}`;
    if (event.kind === "deviation" && event.resolved !== true && !resolved.has(key)) failures.push(`unresolved deviation ${key}`);
    if (event.kind === "checkpoint" && ["blocked", "failed"].includes(event.status) && !resolved.has(key)) failures.push(`unresolved blocker ${key}`);
  }
  for (const command of (section.workOrder.commands || []).filter((item) => item.required !== false)) {
    const started = now();
    const result = await runCommand(command.argv, { cwd: path.resolve(root, command.cwd || "."), timeoutMs: command.timeoutMs });
    const status = result.status === "passed" ? "passed" : "failed";
    checks.push({ id: command.id, status, durationMs: result.durationMs ?? now() - started, exitCode: result.exitCode ?? null, timedOut: Boolean(result.timedOut) });
    if (status !== "passed") failures.push(`declared check '${command.id}' did not pass`);
  }
  return { ok: failures.length === 0, failures, checks, reportPath: relative, events, final: final ?? null };
}

export class Engine {
  constructor({ root, kitRoot, backend, model, thinking, piBinary, deps = {}, readOnlySections = new Set() }) {
    this.root = path.resolve(root);
    this.kitRoot = kitRoot || path.join(this.root, ".conductor", "runtime");
    this.backend = backend;
    this.piBinary = piBinary || null;
    const inherited = resolveSessionModel({ model, thinking, env: deps.env || process.env });
    this.model = inherited.model;
    this.thinking = inherited.thinking;
    this.modelSource = inherited.source;
    this.deps = deps;
    this.readOnlySections = readOnlySections;
    this.stateFile = path.join(this.root, ".conductor-state.md");
    this.ownerToken = mintToken();
    this.writeState = createStateWriter(this.stateFile, this.ownerToken);
    this.children = new Map();
    this.timings = { coordinatorMs: 0, launchMs: 0, dependencyWaitMs: 0, verificationMs: 0, recoveryMs: 0, reworkMs: 0 };
    this.usage = { inputTokens: 0, outputTokens: 0 };
    this.usageKnown = false;
    this.events = [];
    this.runtime = null;
  }

  get lockFile() { return path.join(this.root, ".conductor", "engine.lock"); }
  get guardPath() { return path.join(this.root, ".pi", "extensions", "conductor-guard.ts"); }

  read() { return readState(this.stateFile).state; }

  eventFile(runId) { return path.join(this.root, ".conductor", "runs", runId, "events.jsonl"); }

  emit(state, event) {
    const record = appendEvent(this.eventFile(state.run.id), event);
    this.events.push(record);
    return record;
  }

  acquireLock() {
    fs.mkdirSync(path.dirname(this.lockFile), { recursive: true });
    if (fs.existsSync(this.lockFile)) {
      const owner = JSON.parse(fs.readFileSync(this.lockFile, "utf8"));
      // Refuse any lock we do not own whose holder is alive — including a second
      // Engine instance in this same process. Only a provably dead owner is adopted.
      if (owner.token !== this.ownerToken && (!owner.pid || processAlive(owner.pid))) {
        throw new Error(`engine lock is held by ${owner.pid ? `live pid ${owner.pid}` : "another engine"}; run 'engine recover' if that run is dead`);
      }
      this.staleLock = owner;
    }
    atomicWrite(this.lockFile, `${JSON.stringify({ schemaVersion: 1, token: this.ownerToken, pid: process.pid, startedAt: new Date().toISOString() }, null, 2)}\n`);
    const reread = JSON.parse(fs.readFileSync(this.lockFile, "utf8"));
    if (reread.token !== this.ownerToken) throw new Error("engine lock postcondition failed");
    return reread;
  }

  releaseLock() {
    if (!fs.existsSync(this.lockFile)) return false;
    const owner = JSON.parse(fs.readFileSync(this.lockFile, "utf8"));
    if (owner.token !== this.ownerToken) return false;
    fs.unlinkSync(this.lockFile);
    return true;
  }

  async patch(state, patch, toStatus) {
    const started = now(this.deps);
    const result = await this.writeState({ expectedRevision: state.revision, expectedStatus: state.run.status, patch, toStatus });
    this.timings.coordinatorMs += now(this.deps) - started;
    this.emit(result.state, { kind: "state-write", status: "verified", revision: result.state.revision, to: toStatus ?? null });
    return result.state;
  }

  requireRuntime() {
    if (this.runtime) return this.runtime;
    const inspection = inspectRuntime({ root: this.root, kitRoot: this.kitRoot, piBinary: this.piBinary });
    if (!inspection.ok) throw new Error(`execution runtime is unavailable: ${inspection.errors.join("; ")}`);
    if (!this.model) throw new Error("engine needs a model from the parent Pi session (PI_PROVIDER/PI_MODEL) or --model; there is no hidden catalog default");
    this.runtime = inspection;
    return inspection;
  }

  heartbeatPath(state, section, piRunId) {
    return path.join(this.root, ".conductor", "runs", state.run.id, "agents", section.agent, piRunId, "heartbeat.json");
  }

  writeHeartbeat(state, child) {
    atomicWrite(this.heartbeatPath(state, child.section, child.piRunId), `${JSON.stringify({
      schemaVersion: 1, runId: state.run.id, section: child.section.id, agent: child.section.agent, piRunId: child.piRunId,
      pid: child.peer?.pid ?? null, alive: Boolean(child.peer && !child.peer.exited), at: new Date().toISOString(),
      elapsedMs: now(this.deps) - child.startedAt, source: "engine-runtime",
    }, null, 2)}\n`);
  }

  // Launch one section: mint identity, CAS it into state, authorize the marker,
  // then spawn. Identity exists before the child process does, so there is no
  // window where an unbound child can write.
  async launch(state, sectionId) {
    const started = now(this.deps);
    const section = state.sections[sectionId];
    const piRunId = mintRunId();
    const token = mintToken();
    // The digest covers run/map/agent/deps/work-order/contracts only, so it is
    // identical before and after the status patch. Compute it first so identity,
    // token, and immutable authorization all land in ONE state write, before the
    // child process exists.
    const authorizationDigest = workOrderDigest(state, sectionId);
    const attempt = {
      piRunId, token, authorizationDigest, startedAt: new Date().toISOString(), attempt: (section.attempts?.length || 0) + 1,
      resumable: true, freshFallback: Boolean(section.attempts?.length), acceptance: { status: "pending" }, checks: [],
    };
    const bound = await this.patch(state, { sections: { [sectionId]: { status: "in-progress", attempts: [...(section.attempts || []), attempt] } } });
    const declaredFiles = [...bound.sections[sectionId].workOrder.declaredFiles].sort();
    const execution = { section: sectionId, agent: section.agent, piRunId, token, authorizationDigest, declaredFiles };
    authorizeExecutions({ root: this.root, state: bound, add: [execution], phase: "build" });
    let packet;
    try { packet = buildContextPacket({ root: this.root, state: bound, sectionId, piRunId }); }
    catch (error) { await this.failSection(bound, sectionId, piRunId, `context packet failed: ${error.message}`); return null; }
    const invocation = resolveChildInvocation(this, bound, sectionId, piRunId, token);
    let launched;
    try {
      launched = await launchManagedChild({ backend: this.backend, invocation, expect: { agent: section.agent, piRunId, guardVersion: this.runtime.guardVersion, tools: invocation.tools } });
    } catch (error) {
      this.emit(bound, { kind: "failure", scope: "launch", section: sectionId, recovered: false, message: error.message });
      await this.failSection(bound, sectionId, piRunId, `launch failed: ${error.message}`);
      return null;
    }
    const child = {
      sectionId, section: bound.sections[sectionId], piRunId, token, peer: launched.peer, startedAt: now(this.deps),
      softDeadline: now(this.deps) + softBudgetMs(bound.sections[sectionId]), steered: false, graceDeadline: null,
      controlFile: this.controlPath(bound, sectionId, piRunId), settled: false,
    };
    atomicWrite(child.controlFile, `${JSON.stringify({ schemaVersion: 1, runId: bound.run.id, section: sectionId, agent: section.agent, piRunId, pid: launched.peer.pid, startedAt: new Date().toISOString(), softDeadline: new Date(child.softDeadline).toISOString() }, null, 2)}\n`);
    child.attention = watchChild(child, bound.sections[sectionId].workOrder.budgets);
    this.children.set(sectionId, child);
    this.writeHeartbeat(bound, child);
    this.emit(bound, { kind: "launch", section: sectionId, agent: section.agent, piRunId, freshFallback: attempt.freshFallback });
    this.timings.launchMs += now(this.deps) - started;
    // Send the work order only after readiness is proven.
    child.peer.request({ type: "prompt", message: childPrompt(bound, sectionId, piRunId, packet) }, 60000).catch((error) => { child.promptError = error; });
    return child;
  }

  controlPath(state, sectionId, piRunId) {
    return path.join(this.root, ".conductor", "runs", state.run.id, "executions", `${sectionId}-${piRunId}.json`);
  }

  async failSection(state, sectionId, piRunId, message) {
    const section = state.sections[sectionId];
    const attempts = [...(section.attempts || [])];
    const last = { ...attempts.at(-1), acceptance: { status: "rejected", reason: message }, outcome: "failed", finishedAt: new Date().toISOString(), revokedAt: new Date().toISOString() };
    attempts[attempts.length - 1] = last;
    this.deauthorize(state, sectionId);
    const patched = await this.patch(state, { sections: { [sectionId]: { status: "failed", attempts } } });
    this.emit(patched, { kind: "failure", scope: "section", section: sectionId, piRunId, recovered: false, message });
    return patched;
  }

  deauthorize(state, sectionId) {
    try { return deauthorizeExecution({ root: this.root, state, sectionId }); } catch (error) { this.emit(state, { kind: "failure", scope: "deauthorize", section: sectionId, recovered: false, message: error.message }); return null; }
  }

  // Budget handling: steer, then grace, then abort, then kill. Never `stop`-style
  // destruction of resumability, and never an unbounded wait on a wedged peer.
  async policeBudgets(state) {
    for (const [sectionId, child] of this.children) {
      if (child.settled) continue;
      const budgets = child.section.workOrder.budgets;
      this.writeHeartbeat(state, child);
      if (!child.steered && now(this.deps) >= child.softDeadline) {
        child.steered = true;
        this.emit(state, { kind: "timeout", scope: "phase", section: sectionId, piRunId: child.piRunId });
        child.peer.request({ type: "prompt", message: STEER_MESSAGE, streamingBehavior: "steer" }, 30000).catch(() => null);
        child.graceDeadline = now(this.deps) + budgets.graceMs;
      } else if (child.steered && child.graceDeadline && now(this.deps) >= child.graceDeadline) {
        this.emit(state, { kind: "timeout", scope: "run", section: sectionId, piRunId: child.piRunId });
        await child.peer.abort(10000).catch(() => null);
        await child.peer.close(5000).catch(() => null);
        child.killed = true;
      }
    }
  }

  async harvest(state) {
    let current = state;
    for (const [sectionId, child] of [...this.children]) {
      if (!child.settled && !child.killed && !child.peer.exited && !child.attentionDone) continue;
      if (!child.settled) { child.settled = true; }
      this.children.delete(sectionId);
      const started = now(this.deps);
      const usage = await collectUsage(child.peer);
      if (usage.usageKnown) { this.usage.inputTokens += usage.usage.inputTokens; this.usage.outputTokens += usage.usage.outputTokens; this.usageKnown = true; this.emit(current, { kind: "usage", piRunId: child.piRunId, ...usage.usage }); }
      await child.peer.close(5000).catch(() => null);
      this.timings.verificationMs += now(this.deps) - started;
      const acceptanceStarted = now(this.deps);
      const evaluation = await evaluateAcceptance({ root: this.root, state: current, sectionId, piRunId: child.piRunId });
      this.timings.verificationMs += now(this.deps) - acceptanceStarted;
      const section = current.sections[sectionId];
      const attempts = [...(section.attempts || [])];
      const finishedAt = new Date().toISOString();
      const outcome = evaluation.ok ? "completed" : child.killed ? "interrupted" : "failed";
      attempts[attempts.length - 1] = {
        ...attempts.at(-1), reportPath: evaluation.reportPath, checks: evaluation.checks, outcome, finishedAt,
        revokedAt: finishedAt, durationMs: now(this.deps) - child.startedAt, killed: Boolean(child.killed),
        acceptance: evaluation.ok ? { status: "verified" } : { status: "rejected", reasons: evaluation.failures },
      };
      const status = evaluation.ok ? "done" : "failed";
      // Deauthorize BEFORE the status patch: a finished section left in the marker
      // would fail agreement and block its still-running siblings. Removing it
      // first keeps authorization tight for everyone else without a dead window.
      this.deauthorize(current, sectionId);
      current = await this.patch(current, { sections: { [sectionId]: { status, attempts } } });
      if (evaluation.ok) this.emit(current, { kind: "acceptance", section: sectionId, piRunId: child.piRunId, status: "verified", durationMs: now(this.deps) - child.startedAt });
      else { this.timings.reworkMs += now(this.deps) - child.startedAt; this.emit(current, { kind: "failure", scope: "acceptance", section: sectionId, piRunId: child.piRunId, recovered: false, reasons: evaluation.failures }); }
    }
    return current;
  }

  // Phase 1: identity-bound cartographer. Never use a bare subagent for this —
  // PI_SUBAGENT_RUN_ID must exist before the child can write its candidate.
  async map({ task, timeoutMs, budgets } = {}) {
    const mapBudgets = resolveMapBudgets(budgets);
    const outerMs = timeoutMs ?? (mapBudgets.mapMs + mapBudgets.graceMs + 10000);
    this.requireRuntime();
    if (!this.backend) throw new Error("engine map requires a backend");
    this.acquireLock();
    const started = now(this.deps);
    let state = this.read();
    if (!["idle", "map-done"].includes(state.run.status)) throw new Error(`engine map requires idle or map-done, found '${state.run.status}'`);
    try {
      if (!state.run.id || !state.run.task || !Number.isFinite(Date.parse(state.run.startedAt))) {
        const runId = state.run.id || mintRunId();
        const nextTask = task || state.run.task;
        if (!nextTask) throw new Error("engine map requires a task (--task or state.run.task)");
        state = await this.patch(state, { run: { id: runId, task: nextTask, startedAt: state.run.startedAt || new Date().toISOString(), outcome: state.run.outcome || "pending" } });
      } else if (task && task !== state.run.task) {
        state = await this.patch(state, { run: { task } });
      }
      const piRunId = mintRunId();
      const token = mintToken();
      const authorizationDigest = mapperDigest(state);
      const execution = { section: "cartographer", agent: "cartographer", piRunId, token, authorizationDigest, declaredFiles: [] };
      authorizeExecutions({ root: this.root, state, add: [execution], phase: "map" });
      const invocation = resolveInvocation({
        root: this.root, piBinary: this.runtime.binary, guardPath: this.runtime.guardPath,
        agent: "cartographer", piRunId, token, model: this.model, thinking: this.thinking, role: "mapper",
      });
      let launched;
      try {
        launched = await launchManagedChild({ backend: this.backend, invocation, expect: { agent: "cartographer", piRunId, guardVersion: this.runtime.guardVersion, tools: invocation.tools } });
      } catch (error) {
        deauthorizeExecution({ root: this.root, state, sectionId: "cartographer" });
        throw new Error(`cartographer launch failed: ${error.message}`);
      }
      const candidateRel = `.conductor/runs/${state.run.id}/agents/cartographer/${piRunId}/section-map.candidate.json`;
      const candidateAbs = path.join(this.root, candidateRel);
      const reportPath = expectedReportPath(this.root, state.run.id, "cartographer", piRunId);
      const child = {
        sectionId: "cartographer",
        section: { id: "cartographer", agent: "cartographer", workOrder: { budgets: { exploreMs: mapBudgets.mapMs, buildMs: 1, verifyMs: 1, graceMs: mapBudgets.graceMs, heartbeatMs: mapBudgets.heartbeatMs } } },
        piRunId, token, peer: launched.peer, startedAt: now(this.deps),
        softDeadline: now(this.deps) + mapBudgets.mapMs,
        steered: false, graceDeadline: null, settled: false,
      };
      child.attention = watchChild(child);
      this.children.set("cartographer", child);
      this.writeHeartbeat(state, child);
      this.emit(state, { kind: "launch", section: "cartographer", agent: "cartographer", piRunId, budgets: mapBudgets });
      child.peer.request({ type: "prompt", message: mapperPrompt(state, piRunId, this.root) }, 60000).catch((error) => { child.promptError = error; });
      const deadline = now(this.deps) + outerMs;
      let lastProgressAt = now(this.deps); let lastSig = mapperProgressSignature(reportPath, candidateAbs);
      while (!child.attentionDone && !child.peer.exited && now(this.deps) < deadline) {
        if (fs.existsSync(candidateAbs)) break;
        const sig = mapperProgressSignature(reportPath, candidateAbs);
        if (sig !== lastSig) { lastSig = sig; lastProgressAt = now(this.deps); }
        this.writeHeartbeat(state, child);
        if (!child.steered && now(this.deps) - lastProgressAt >= mapBudgets.stallMs) {
          child.steered = true;
          this.emit(state, { kind: "timeout", scope: "stall", section: "cartographer", piRunId, idleMs: now(this.deps) - lastProgressAt });
          child.peer.request({ type: "prompt", message: STEER_MESSAGE, streamingBehavior: "steer" }, 15000).catch(() => null);
          child.graceDeadline = now(this.deps) + mapBudgets.graceMs;
        } else if (child.steered && child.graceDeadline && now(this.deps) >= child.graceDeadline) {
          this.emit(state, { kind: "timeout", scope: "run", section: "cartographer", piRunId });
          await child.peer.abort(10000).catch(() => null);
          await child.peer.close(5000).catch(() => null);
          break;
        } else if (!child.steered && now(this.deps) - child.startedAt >= mapBudgets.mapMs) {
          child.steered = true;
          this.emit(state, { kind: "timeout", scope: "phase", section: "cartographer", piRunId });
          child.peer.request({ type: "prompt", message: STEER_MESSAGE, streamingBehavior: "steer" }, 15000).catch(() => null);
          child.graceDeadline = now(this.deps) + mapBudgets.graceMs;
        }
        await Promise.race([child.attention, sleep(TICK_MS, this.deps)]);
      }
      if (!child.peer.exited) {
        if (fs.existsSync(candidateAbs) && !child.attentionDone) await child.peer.abort(5000).catch(() => null);
        await child.peer.close(5000).catch(() => null);
      }
      this.children.delete("cartographer");
      deauthorizeExecution({ root: this.root, state, sectionId: "cartographer" });
      if (!fs.existsSync(candidateAbs)) throw new Error(missingCandidateMessage({ root: this.root, runId: state.run.id, piRunId, candidateRel }));
      const map = JSON.parse(fs.readFileSync(candidateAbs, "utf8"));
      const candidates = listInventory(this.root).filter((file) => !(map.ignored || []).some((glob) => { try { return resolveOwnership({ rules: [{ owner: "ignored", glob }] }, file).kind === "owned"; } catch { return false; } }));
      const agents = fs.existsSync(path.join(this.root, ".pi", "agents")) ? fs.readdirSync(path.join(this.root, ".pi", "agents")).filter((name) => name.endsWith(".md")).map((name) => path.basename(name, ".md")) : [];
      const check = validateMap(map, { candidates, availableAgents: agents });
      if (!check.ok) throw new Error(`cartographer candidate failed validation: ${check.errors.join("; ")}`);
      const mapFile = path.join(this.root, state.map.path || ".conductor/section-map.json");
      atomicWrite(mapFile, `${JSON.stringify(map, null, 2)}\n`);
      const sha256 = crypto.createHash("sha256").update(fs.readFileSync(mapFile)).digest("hex");
      const validatedAt = new Date().toISOString();
      const toStatus = state.run.status === "idle" ? "map-done" : undefined;
      state = await this.patch(state, { map: { path: state.map.path || ".conductor/section-map.json", sha256, validatedAt } }, toStatus);
      this.emit(state, { kind: "phase", phase: "map", event: "end", durationMs: now(this.deps) - started, candidate: candidateRel });
      return { ok: true, state, candidate: candidateRel, sha256, waves: check.waves, piRunId };
    } finally {
      for (const child of this.children.values()) await child.peer.close(5000).catch(() => null);
      this.children.clear();
      this.releaseLock();
    }
  }

  async build({ maxConcurrency, task } = {}) {
    const runtime = this.requireRuntime();
    if (!this.backend) throw new Error("engine build requires a backend (process backend by default)");
    this.acquireLock();
    const started = now(this.deps);
    let state = this.read();
    if (!["plan-done", "build-done"].includes(state.run.status)) throw new Error(`engine build requires plan-done, found '${state.run.status}'`);
    if (state.run.status === "build-done") throw new Error("build is already complete; run verify/review instead");
    const concurrency = maxConcurrency ?? state.scheduling?.maxConcurrency ?? 3;
    if (state.scheduling?.mode !== "ready") throw new Error("engine build requires state.scheduling.mode 'ready'; legacy wave runs use the coordinator protocol");
    if (!task && !state.run.task) throw new Error("engine build requires a task in state");
    this.emit(state, { kind: "phase", phase: "build", event: "start" });
    const waitStart = now(this.deps);
    try {
      while (true) {
        state = this.read();
        if (allSettled(state)) break;
        const failed = Object.entries(state.sections).filter(([, section]) => section.status === "failed");
        if (failed.length && !this.children.size) throw new Error(`build stopped: section(s) failed mechanical acceptance: ${failed.map(([id]) => id).join(", ")}`);
        const selection = readySections(state, { maxConcurrency: concurrency, inFlight: new Set(this.children.keys()) });
        if (!selection.ready.length && !this.children.size) throw new Error("build is wedged: no section is ready and none is in flight");
        for (const sectionId of selection.ready) { const launched = await this.launch(state, sectionId); if (launched) state = this.read(); }
        if (this.children.size) {
          const racers = [...this.children.values()].map((child) => child.attention);
          racers.push(sleep(TICK_MS, this.deps));
          await Promise.race(racers);
          await this.policeBudgets(state);
          for (const child of this.children.values()) if (child.attentionDone) child.settled = true;
          state = await this.harvest(state);
        }
      }
      state = this.read();
      this.timings.dependencyWaitMs = Math.max(0, now(this.deps) - waitStart - this.timings.launchMs - this.timings.verificationMs);
      // An engine-driven run must clear exactly the same promotion gates as a
      // coordinator-driven run: settled authorization, plan agreement, and
      // per-writer mechanical evidence. Never promote on schedule alone.
      assertBuildSettled(this.root);
      assertPlanAgreement(state, JSON.parse(fs.readFileSync(path.resolve(this.root, state.map.path), "utf8")));
      assertWriterEvidence(state, this.root);
      state = await this.patch(state, {}, "build-done");
      this.emit(state, { kind: "phase", phase: "build", event: "end", durationMs: now(this.deps) - started });
      this.emit(state, { kind: "timing-summary", ...this.timings });
      return { ok: true, state, timings: { ...this.timings }, usageKnown: this.usageKnown, usage: { ...this.usage }, sections: summarize(state) };
    } finally {
      for (const child of this.children.values()) await child.peer.close(5000).catch(() => null);
      this.children.clear();
      this.releaseLock();
    }
  }

  status() {
    const state = this.read();
    const markerFile = path.join(this.root, ".conductor", "active.json");
    const marker = fs.existsSync(markerFile) ? JSON.parse(fs.readFileSync(markerFile, "utf8")) : null;
    const inFlight = new Set((marker?.executions || []).map((item) => item.section));
    const selection = readySections(state, { maxConcurrency: state.scheduling?.maxConcurrency ?? 3, inFlight });
    return {
      runId: state.run.id, status: state.run.status, revision: state.revision, scheduling: state.scheduling ?? null,
      model: this.model, modelSource: this.modelSource, thinking: this.thinking,
      marker: marker ? { authorization: marker.authorization ?? "revision", phase: marker.phase, executions: marker.executions.map((item) => ({ section: item.section, agent: item.agent, piRunId: item.piRunId ?? null })) } : null,
      sections: summarize(state), ready: selection.ready, blockedByCapacity: selection.blocked,
      staleExecutions: (marker?.executions || []).filter((item) => !isExecutionAlive(this.root, state, item.section, item.piRunId)).map((item) => ({ section: item.section, piRunId: item.piRunId })),
    };
  }

  // Recovery across a parent boundary: revoke dead identities first, then relaunch
  // fresh with checkpoints. A live child is never double-run.
  async recover({ relaunch = false, maxConcurrency } = {}) {
    const started = now(this.deps);
    this.acquireLock();
    try {
      let state = this.read();
      const markerFile = path.join(this.root, ".conductor", "active.json");
      const recovered = [];
      if (fs.existsSync(markerFile)) {
        const marker = JSON.parse(fs.readFileSync(markerFile, "utf8"));
        for (const execution of marker.executions || []) {
          const section = state.sections[execution.section];
          if (!section) continue;
          if (isExecutionAlive(this.root, state, execution.section, execution.piRunId)) { recovered.push({ section: execution.section, piRunId: execution.piRunId, action: "left-running" }); continue; }
          const attempts = [...(section.attempts || [])];
          const index = attempts.findIndex((item) => item.piRunId === execution.piRunId);
          if (index >= 0) attempts[index] = { ...attempts[index], outcome: "interrupted", revokedAt: new Date().toISOString(), acceptance: { status: "rejected", reasons: ["execution identity revoked during recovery"] }, finishedAt: new Date().toISOString() };
          state = await this.patch(state, { sections: { [execution.section]: { status: "pending", attempts } } });
          this.deauthorize(state, execution.section);
          this.emit(state, { kind: "resume", section: execution.section, piRunId: execution.piRunId, status: "revoked", freshFallback: true });
          recovered.push({ section: execution.section, piRunId: execution.piRunId, action: "revoked-and-reset" });
        }
        if (fs.existsSync(markerFile)) {
          const remaining = JSON.parse(fs.readFileSync(markerFile, "utf8"));
          if (!(remaining.executions || []).length) fs.unlinkSync(markerFile);
        }
      }
      this.timings.recoveryMs += now(this.deps) - started;
      if (relaunch) {
        const result = await this.build({ maxConcurrency });
        return { recovered, relaunched: true, ...result };
      }
      return { recovered, relaunched: false, state: this.read(), timings: { ...this.timings } };
    } finally { this.releaseLock(); }
  }
}

function summarize(state) {
  return Object.entries(state.sections || {}).map(([id, section]) => ({ id, agent: section.agent, status: section.status, dependsOn: section.dependsOn, attempts: (section.attempts || []).length, acceptance: section.attempts?.at(-1)?.acceptance?.status ?? null }));
}

function processAlive(pid) { if (!Number.isSafeInteger(pid) || pid <= 0) return false; try { process.kill(pid, 0); return true; } catch (error) { return error.code === "EPERM"; } }

function isExecutionAlive(root, state, sectionId, piRunId) {
  if (!piRunId) return false;
  const controlDir = path.join(root, ".conductor", "runs", state.run.id, "executions");
  if (!fs.existsSync(controlDir)) return false;
  const file = path.join(controlDir, `${sectionId}-${piRunId}.json`);
  if (!fs.existsSync(file)) return false;
  try { return processAlive(JSON.parse(fs.readFileSync(file, "utf8")).pid); } catch { return false; }
}

function softBudgetMs(section) {
  const budgets = section.workOrder.budgets;
  return (budgets.exploreMs || 0) + (budgets.buildMs || 0) + (budgets.verifyMs || 0);
}

// Resolve when the child settles or its process exits. Budget enforcement stays
// in policeBudgets, so a wedged peer can never hold the run open forever.
function watchChild(child) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (reason) => { if (done) return; done = true; child.attentionDone = true; child.settleReason = reason; resolve(reason); };
    child.peer.onEvent((event) => {
      if (event.type === "agent_settled") finish("settled");
      else if (event.type === "__peer_exit") finish("exit");
      else if (event.type === "extension_error") child.extensionErrors = [...(child.extensionErrors || []), event.error];
    });
  });
}

// Live grok-4.6 runs exhausted 270s while writing the candidate after inventory.
// Defaults leave room for a slow model; CLI flags can tighten them.
// Mapper waits for progress, not a 22-minute brick. Stall cuts idle children;
// the hard cap is mapMs. A written candidate ends the wait immediately.
export const MAP_BUDGETS = { mapMs: 240000, stallMs: 90000, graceMs: 20000, heartbeatMs: 5000 };

export function resolveMapBudgets(overrides = {}) {
  const base = { ...MAP_BUDGETS, ...overrides };
  if (overrides.mapMs == null && [overrides.exploreMs, overrides.buildMs, overrides.verifyMs].some((value) => Number.isFinite(value))) {
    base.mapMs = (overrides.exploreMs || 0) + (overrides.buildMs || 0) + (overrides.verifyMs || 0);
  }
  for (const key of ["mapMs", "stallMs", "graceMs", "heartbeatMs"]) {
    if (!Number.isSafeInteger(base[key]) || base[key] <= 0) throw new Error(`map budget ${key} must be a positive integer`);
  }
  return base;
}

function mapperProgressSignature(reportPath, candidateAbs) {
  const report = fs.existsSync(reportPath) ? `${fs.statSync(reportPath).size}:${fs.statSync(reportPath).mtimeMs}` : "0";
  const candidate = fs.existsSync(candidateAbs) ? `${fs.statSync(candidateAbs).size}` : "0";
  return `${report}:${candidate}`;
}

export function missingCandidateMessage({ root, runId, piRunId, candidateRel }) {
  const bound = `cartographer produced no candidate at ${candidateRel} (identity was bound as ${piRunId})`;
  try {
    const events = readJsonl(expectedReportPath(root, runId, "cartographer", piRunId));
    const last = events.at(-1);
    if (!last) return `${bound}. no child checkpoint was written`;
    const bits = [last.kind, last.phase, last.unit, last.status, last.next].filter((value) => value !== undefined && value !== "");
    return `${bound}. last checkpoint: ${bits.join(" / ")}`;
  } catch {
    return `${bound}. no readable child report`;
  }
}

function listInventory(root) {
  const top = spawnSync("git", ["rev-parse", "--show-toplevel"], { cwd: root, encoding: "utf8" });
  if (top.status === 0 && path.resolve(top.stdout.trim()) === path.resolve(root)) {
    const result = spawnSync("git", ["ls-files", "--cached", "--others", "--exclude-standard", "-z"], { cwd: root, encoding: "utf8" });
    if (result.status === 0) return result.stdout.split("\0").filter(Boolean).sort();
  }
  const found = [];
  const visit = (directory) => {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      if ([".git", "node_modules", ".conductor", ".vnodes", ".pi"].includes(entry.name)) continue;
      const absolute = path.join(directory, entry.name);
      if (entry.isDirectory()) visit(absolute);
      else if (entry.isFile()) found.push(path.relative(root, absolute).split(path.sep).join("/"));
    }
  };
  visit(root);
  return found.sort();
}

export const STEER_MESSAGE = "PHASE BUDGET EXHAUSTED. STOP exploring after the current tool. Append a checkpoint containing completed and remaining units, changed files, command state, and next action. Finish only the current smallest safe unit, then append your final event and return.";

export function childPrompt(state, sectionId, piRunId, packet) {
  const section = state.sections[sectionId];
  const acceptance = (section.workOrder.doneWhen || []).map((item, index) => `${index + 1}. ${typeof item === "string" ? item : JSON.stringify(item)}`).join("\n");
  return [
    `You are the '${section.agent}' owner for section '${sectionId}' of Conductor run '${state.run.id}'.`,
    `Your child run identity is '${piRunId}'. It is already bound; do not create another.`,
    "",
    "## Task",
    section.workOrder.do?.map?.((item) => `- ${typeof item === "string" ? item : JSON.stringify(item)}`).join("\n") || `- ${JSON.stringify(section.workOrder.do)}`,
    "",
    "## Acceptance (checked mechanically, not by prose)",
    acceptance || "- (none declared)",
    "",
    "## Rules",
    `- Edit ONLY these exact files: ${JSON.stringify(section.workOrder.declaredFiles)}.`,
    "- Use only declared conductor_command IDs for commands; there is no shell.",
    "- Append semantic checkpoints with conductor_report at resumable boundaries, before yielding, on any blocker, and always a final event before returning.",
    "- Runtime liveness heartbeats are written for you; do not spend turns on them.",
    "- A contract you cannot honor is a deviation event plus a blocked final, then return. Never improvise across sections.",
    "",
    "## Work order, contracts, context and checkpoints",
    "```json",
    JSON.stringify(packet),
    "```",
  ].join("\n");
}

export function mapperPrompt(state, piRunId, root) {
  return [
    `You are the cartographer for Conductor run '${state.run.id}'. Your child run identity is '${piRunId}'. It is already bound; do not create another.`,
    "Never edit source, .conductor-state.md, the promoted map, or active state.",
    `Write ONLY in .conductor/runs/${state.run.id}/agents/cartographer/${piRunId}/`,
    "Required files: section-map.candidate.json and a conductor_report checkpoint then final.",
    "If conductor_evidence reports that this is not a git repository, use ls/find/read on the project root. Do not treat empty git output as a blocker.",
    "Emit schema v2. Keep the shipped roster unless the repository clearly needs fewer enabled sections. id must equal agent. Use only agents that exist in .pi/agents.",
    "Every inventory file that is not ignored MUST be owned. Root-level *.txt and *.md belong to docs. Do not leave a candidate unmapped.",
    "Put specific cross-owner rules before broad rules. Empty sections have no rule.",
    `Task: ${state.run.task || "(none)"}`,
    `Project root: ${root}`,
  ].join("\n");
}

function resolveChildInvocation(engine, state, sectionId, piRunId, token) {
  const section = state.sections[sectionId];
  return resolveInvocation({
    // One source of truth: the guard path the runtime inspection actually accepted.
    root: engine.root, piBinary: engine.runtime.binary, guardPath: engine.runtime.guardPath,
    agent: section.agent, piRunId, token, model: engine.model, thinking: engine.thinking,
    readOnly: engine.readOnlySections.has(sectionId),
  });
}
