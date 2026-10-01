import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { readJsonl } from "./report.mjs";

const FORBIDDEN_KEYS = /prompt|transcript|credential|secret|environment|absolutePath|task$/i;
const TELEMETRY_KEYS = new Set(["schemaVersion", "runId", "conductorVersion", "startedAt", "endedAt", "wallClockMs", "taskSha256", "launches", "resumes", "timeouts", "toolHangs", "stateWrites", "usage", "phaseDurationsMs", "mechanicalVerdict", "reviewVerdict", "verdict", "unrecoveredFailures", "timing", "usageKnown", "quality"]);
const TIMING_KEYS = ["coordinatorMs", "launchMs", "dependencyWaitMs", "verificationMs", "recoveryMs", "reworkMs"];
const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

const COUNT_GROUPS = {
  resumes: ["attempted", "succeeded", "freshFallbacks"],
  timeouts: ["phaseBudget", "tool", "run"],
  toolHangs: ["suspected", "confirmedTimeouts"],
  stateWrites: ["attempted", "verified", "failed", "silentNoops"],
  usage: ["inputTokens", "outputTokens", "totalTokens"],
};
const MECHANICAL_VERDICTS = ["passed", "failed", "unknown"];
const REVIEW_VERDICTS = ["accepted", "rejected", "infra-failure", "infra-closed", "needs-human-review", "unknown"];
const RUN_VERDICTS = ["accepted", "rejected", "needs-human-review", "failed"];

function validateCounts(record, errors) {
  for (const [group, keys] of Object.entries(COUNT_GROUPS)) {
    const value = record[group];
    if (!value || typeof value !== "object" || Array.isArray(value)) { errors.push(`${group} must be an object`); continue; }
    for (const key of Object.keys(value)) if (!keys.includes(key)) errors.push(`unknown ${group} key: ${key}`);
    for (const key of keys) if (!Number.isSafeInteger(value[key]) || value[key] < 0) errors.push(`${group}.${key} must be a non-negative integer`);
  }
  if (record.resumes && record.resumes.succeeded > record.resumes.attempted) errors.push("resumes.succeeded cannot exceed resumes.attempted");
  if (record.resumes && record.resumes.freshFallbacks > record.resumes.attempted) errors.push("resumes.freshFallbacks cannot exceed resumes.attempted");
  if (record.toolHangs && record.toolHangs.confirmedTimeouts > record.toolHangs.suspected) errors.push("toolHangs.confirmedTimeouts cannot exceed suspected");
  if (record.stateWrites && record.stateWrites.verified + record.stateWrites.failed > record.stateWrites.attempted) errors.push("stateWrites verified+failed cannot exceed attempted");
  if (record.usage && record.usage.totalTokens !== record.usage.inputTokens + record.usage.outputTokens) errors.push("usage.totalTokens must equal inputTokens + outputTokens");
}

export function validateTelemetry(record) {
  const errors = [];
  if (!record || typeof record !== "object" || Array.isArray(record)) return ["telemetry record must be an object"];
  if (record.schemaVersion !== 1) return ["telemetry schemaVersion must be 1"];
  for (const key of Object.keys(record)) if (!TELEMETRY_KEYS.has(key)) errors.push(`unknown telemetry key: ${key}`);
  for (const key of ["runId", "conductorVersion", "taskSha256"]) if (typeof record[key] !== "string" || !record[key]) errors.push(`${key} is required`);
  for (const key of ["startedAt", "endedAt"]) { const parsed = Date.parse(record[key]); if (!Number.isFinite(parsed)) errors.push(`${key} must be a valid date-time`); }
  if (Number.isFinite(Date.parse(record.startedAt)) && Number.isFinite(Date.parse(record.endedAt)) && Date.parse(record.endedAt) < Date.parse(record.startedAt)) errors.push("endedAt must not precede startedAt");
  if (!MECHANICAL_VERDICTS.includes(record.mechanicalVerdict)) errors.push(`mechanicalVerdict must be one of ${MECHANICAL_VERDICTS.join("|")}`);
  if (!REVIEW_VERDICTS.includes(record.reviewVerdict)) errors.push(`reviewVerdict must be one of ${REVIEW_VERDICTS.join("|")}`);
  if (!RUN_VERDICTS.includes(record.verdict)) errors.push(`verdict must be one of ${RUN_VERDICTS.join("|")}`);
  if (!/^[A-Za-z0-9._-]+$/.test(record.runId || "")) errors.push("runId is unsafe");
  if (!/^[a-f0-9]{64}$/.test(record.taskSha256 || "")) errors.push("taskSha256 must be a SHA-256 digest");
  for (const key of ["wallClockMs", "launches", "unrecoveredFailures"]) if (!Number.isSafeInteger(record[key]) || record[key] < 0) errors.push(`${key} must be a non-negative integer`);
  validateCounts(record, errors);
  if (record.usageKnown !== undefined && typeof record.usageKnown !== "boolean") errors.push("usageKnown must be boolean");
  if (record.timing !== undefined) {
    if (!record.timing || typeof record.timing !== "object" || Array.isArray(record.timing)) errors.push("timing must be an object");
    else for (const [key, value] of Object.entries(record.timing)) if (!TIMING_KEYS.includes(key) || !Number.isFinite(value) || value < 0) errors.push(`invalid timing.${key}`);
  }
  if (record.quality !== undefined) {
    if (!record.quality || typeof record.quality !== "object" || Array.isArray(record.quality)) errors.push("quality must be an object");
    else for (const [key, value] of Object.entries(record.quality)) {
      if (key === "repairRounds" ? !Number.isSafeInteger(value) || value < 0 : !["firstPassAccepted", "recoverySucceeded"].includes(key) || typeof value !== "boolean") errors.push(`invalid quality.${key}`);
    }
  }
  if (record.phaseDurationsMs === undefined || typeof record.phaseDurationsMs !== "object" || Array.isArray(record.phaseDurationsMs)) errors.push("phaseDurationsMs must be an object");
  else { for (const [phase, value] of Object.entries(record.phaseDurationsMs)) { if (!/^[a-z][a-z0-9-]*$/.test(phase)) errors.push(`phaseDurationsMs has unsafe phase name '${phase}'`); if (!Number.isSafeInteger(value) || value < 0) errors.push(`phaseDurationsMs.${phase} must be a non-negative integer`); } }
  const inspect = (value, trail = []) => { if (typeof value === "string" && (/^\/(Users|home|private|var|etc)\//.test(value) || /^[A-Za-z]:[\\/]/.test(value))) errors.push(`privacy-forbidden absolute path at ${trail.join(".")}`); if (!value || typeof value !== "object") return; for (const [key, child] of Object.entries(value)) { if (FORBIDDEN_KEYS.test(key)) errors.push(`privacy-forbidden key: ${[...trail, key].join(".")}`); inspect(child, [...trail, key]); } };
  inspect(record);
  return errors;
}

export function taskDigest(task) { return crypto.createHash("sha256").update(String(task)).digest("hex"); }

function withLock(file, callback) {
  const lock = `${file}.lock`; fs.mkdirSync(path.dirname(file), { recursive: true }); let fd;
  for (let attempt = 0; attempt < 40; attempt += 1) { try { fd = fs.openSync(lock, "wx", 0o600); break; } catch (error) { if (error.code !== "EEXIST" || attempt === 39) throw error; sleep(25); } }
  try { return callback(); } finally { if (fd !== undefined) fs.closeSync(fd); try { fs.unlinkSync(lock); } catch {} }
}

export function appendTelemetry(file, record) {
  const errors = validateTelemetry(record); if (errors.length) throw new Error(errors.join("; "));
  return withLock(file, () => {
    const prior = readJsonl(file); requireValidRecords(prior); if (prior.some((x) => x.runId === record.runId)) throw new Error(`telemetry already contains runId '${record.runId}'`);
    const fd = fs.openSync(file, "a", 0o600); try { fs.writeSync(fd, `${JSON.stringify(record)}\n`); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
    const next = readJsonl(file); if (next.length !== prior.length + 1 || next.at(-1).runId !== record.runId) throw new Error("telemetry append postcondition failed");
    return record;
  });
}

function requireValidRecords(records) {
  const failures = [];
  for (const [index, record] of records.entries()) { const errors = validateTelemetry(record); if (errors.length) failures.push(`record ${index + 1} (${record?.runId || "unknown"}): ${errors.join(", ")}`); }
  if (failures.length) throw new Error(`semantically invalid telemetry:\n${failures.join("\n")}`);
}

export function soak(records, n = 10) {
  if (!Number.isSafeInteger(n) || n < 1) throw new Error("N must be a positive integer");
  requireValidRecords(records);
  if (records.length < n) return { passed: false, reason: `need ${n} records, found ${records.length}`, inspected: records.length };
  const sample = records.slice(-n); const failures = [];
  for (const record of sample) {
    if (record.verdict !== "accepted") failures.push(`${record.runId}: verdict ${record.verdict}`);
    if (record.unrecoveredFailures !== 0) failures.push(`${record.runId}: unrecovered failures`);
    if (record.stateWrites.failed !== 0 || record.stateWrites.attempted !== record.stateWrites.verified) failures.push(`${record.runId}: failed/unverified state writes`);
    if (record.stateWrites.silentNoops !== 0) failures.push(`${record.runId}: silent no-ops`);
    const recoveries = record.resumes.succeeded + record.resumes.freshFallbacks;
    if (record.timeouts.phaseBudget + record.timeouts.run > recoveries) failures.push(`${record.runId}: timeout without successful resume/fallback`);
  }
  return { passed: failures.length === 0, inspected: n, failures };
}

function rate(records) { if (!records.length) return 0; return records.reduce((sum, record) => sum + record.timeouts.phaseBudget + record.timeouts.tool + record.timeouts.run + record.toolHangs.confirmedTimeouts, 0) / records.length; }

function performanceSummary(records) {
  const accepted = records.filter((r) => r.verdict === "accepted");
  const durations = accepted.map((r) => r.wallClockMs).sort((a, b) => a - b);
  const percentile = (p) => durations.length ? durations[Math.ceil(p * durations.length) - 1] : null;
  return { runs: records.length, accepted: accepted.length, nonAccepted: records.length - accepted.length,
    timeToAcceptanceMs: { samples: durations.length, p50: percentile(0.5), p95: percentile(0.95) },
    usageKnownRuns: records.filter((r) => r.usageKnown === true).length,
    usageUnknownRuns: records.filter((r) => r.usageKnown !== true).length };
}

export function trend(records) {
  requireValidRecords(records);
  const performance = { overall: performanceSummary(records), speedRegression: "unknown", reason: "observational timing only; task/workload mix is not controlled" };
  if (records.length < 25) return { alert: false, reason: `need 25 records, found ${records.length}`, performance };
  const latest = records.slice(-5); const previous = records.slice(-25, -5); const latestRate = rate(latest); const previousRate = rate(previous);
  performance.latest = performanceSummary(latest); performance.previous = performanceSummary(previous);
  return { alert: latestRate > 0.1 && latestRate - previousRate >= 0.1, latestRate, previousRate, delta: latestRate - previousRate, performance };
}

export function telemetryFromEvents({ runId, conductorVersion, task, startedAt, endedAt, events, usage }) {
  const count = (kind) => events.filter((event) => event.kind === kind).length;
  const resumes = events.filter((event) => event.kind === "resume");
  const stateWrites = events.filter((event) => event.kind === "state-write");
  const usageEvents = events.filter((event) => event.kind === "usage");
  const validUsage = (u) => u && [u.inputTokens, u.outputTokens].every((n) => Number.isSafeInteger(n) && n >= 0);
  const sources = usage !== undefined ? [usage] : usageEvents;
  const usageKnown = sources.length > 0 && sources.every(validUsage);
  const tokens = sources.filter(validUsage).reduce((sum, u) => ({ inputTokens: sum.inputTokens + u.inputTokens, outputTokens: sum.outputTokens + u.outputTokens }), { inputTokens: 0, outputTokens: 0 });
  // Phase names are validated as /^[a-z][a-z0-9-]*$/, so a plain object cannot be
  // reached by a prototype-polluting key; it also survives JSON round-trips cleanly.
  const phaseDurationsMs = {}; const timing = {}; const quality = {};
  for (const event of events) {
    if (!Number.isFinite(event.durationMs) || event.durationMs < 0) continue;
    if (event.kind === "phase" && /^[a-z][a-z0-9-]*$/.test(event.phase)) phaseDurationsMs[event.phase] = (phaseDurationsMs[event.phase] || 0) + Math.floor(event.durationMs);
    if (event.kind === "timing" && TIMING_KEYS.includes(event.name)) timing[event.name] = (timing[event.name] || 0) + event.durationMs;
  }
  const reviews = events.filter((e) => e.kind === "review" && ["accepted", "rejected"].includes(e.verdict));
  const mechanical = events.filter((e) => e.kind === "mechanical" && ["passed", "failed"].includes(e.verdict));
  const terminal = events.findLast((e) => e.kind === "terminal");
  if (reviews.length && mechanical.length && terminal) quality.firstPassAccepted = terminal.verdict === "accepted" && reviews.every((e) => e.verdict === "accepted") && mechanical.every((e) => e.verdict === "passed");
  // Repair rounds require explicit runtime events; rejected reviews are not necessarily repairs.
  if (events.some((e) => e.kind === "repair")) quality.repairRounds = count("repair");
  const failures = events.filter((e) => e.kind === "failure");
  if (failures.length && failures.every((e) => typeof e.recovered === "boolean")) quality.recoverySucceeded = failures.every((e) => e.recovered);
  return {
    schemaVersion: 1, runId, conductorVersion, startedAt, endedAt,
    wallClockMs: Date.parse(endedAt) - Date.parse(startedAt), taskSha256: taskDigest(task),
    launches: count("launch"),
    resumes: { attempted: resumes.length, succeeded: resumes.filter((x) => x.status === "succeeded").length, freshFallbacks: resumes.filter((x) => x.freshFallback).length },
    timeouts: { phaseBudget: events.filter((x) => x.kind === "timeout" && x.scope === "phase").length, tool: events.filter((x) => x.kind === "timeout" && x.scope === "tool").length, run: events.filter((x) => x.kind === "timeout" && x.scope === "run").length },
    toolHangs: { suspected: count("tool-hang-suspected"), confirmedTimeouts: count("tool-hang-timeout") },
    stateWrites: { attempted: stateWrites.length, verified: stateWrites.filter((x) => x.status === "verified").length, failed: stateWrites.filter((x) => x.status === "failed").length, silentNoops: stateWrites.filter((x) => x.status === "noop").length },
    usage: { ...tokens, totalTokens: tokens.inputTokens + tokens.outputTokens }, usageKnown,
    phaseDurationsMs, ...(Object.keys(timing).length ? { timing } : {}), ...(Object.keys(quality).length ? { quality } : {}),
    mechanicalVerdict: events.findLast((x) => x.kind === "mechanical")?.verdict || "unknown",
    reviewVerdict: events.findLast((x) => x.kind === "review")?.verdict || "unknown",
    verdict: events.findLast((x) => x.kind === "terminal")?.verdict || "failed",
    unrecoveredFailures: events.filter((x) => x.kind === "failure" && !x.recovered).length,
  };
}
