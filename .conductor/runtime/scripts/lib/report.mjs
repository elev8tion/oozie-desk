import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { safeTarget } from "./context-paths.mjs";

const histories = new Map();
const hash = (data) => crypto.createHash("sha256").update(data).digest("hex");
const stamp = (stat) => `${stat.dev}:${stat.ino}:${stat.size}:${stat.mtimeNs}:${stat.ctimeNs}`;
function validatedHistory(file) {
  if (!fs.existsSync(file)) { histories.delete(file); return []; }
  const stat = fs.statSync(file, { bigint: true }), data = fs.readFileSync(file), sha256 = hash(data);
  const cached = histories.get(file);
  if (cached?.stamp === stamp(stat) && cached.sha256 === sha256) return cached.events;
  const events = readJsonl(file); let previous;
  for (const [index, event] of events.entries()) {
    const errors = validateReportEvent(event);
    if (errors.length || event.seq !== index + 1 || previous?.kind === "final" || (previous && Date.parse(event.at) < Date.parse(previous.at))) throw new Error(`existing report is invalid: ${errors.join("; ") || "sequence/final/timestamp violation"}`);
    previous = event;
  }
  if (data.length && data.at(-1) !== 10) throw new Error("existing report is invalid: missing newline");
  if (stamp(fs.statSync(file, { bigint: true })) !== stamp(stat) || hash(fs.readFileSync(file)) !== sha256) throw new Error("report changed during validation");
  histories.set(file, { stamp: stamp(stat), sha256, events }); return events;
}
import { normalizeRelative } from "../../extension/conductor-ownership.mjs";

export function validateReportEvent(event) {
  const errors = [];
  if (!event || typeof event !== "object" || Array.isArray(event)) return ["event must be an object"];
  if (event.schemaVersion !== 1) errors.push("schemaVersion must be 1");
  if (!Number.isSafeInteger(event.seq) || event.seq < 1) errors.push("seq must be a positive integer");
  if (!Number.isFinite(Date.parse(event.at))) errors.push("at must be an ISO timestamp");
  if (!["checkpoint", "deviation", "final"].includes(event.kind)) errors.push("invalid event kind");
  if (event.heartbeatMissed !== undefined && typeof event.heartbeatMissed !== "boolean") errors.push("heartbeatMissed must be a boolean");
  if (event.heartbeatMissedByMs !== undefined && (!Number.isSafeInteger(event.heartbeatMissedByMs) || event.heartbeatMissedByMs <= 0)) errors.push("heartbeatMissedByMs must be a positive integer");
  if (event.kind === "checkpoint") {
    if (!["explore", "build", "verify", "finalize"].includes(event.phase)) errors.push("checkpoint phase is invalid");
    if (typeof event.unit !== "string" || !event.unit) errors.push("checkpoint unit is required");
    if (!Array.isArray(event.files)) errors.push("checkpoint files must be an array");
    if (typeof event.next !== "string") errors.push("checkpoint next is required");
  }
  if (event.kind === "deviation" && (!event.contractId || !event.reason || !event.requestedChange)) errors.push("deviation requires contractId, reason, requestedChange");
  if (event.kind === "final") {
    if (!["completed", "blocked", "failed", "interrupted"].includes(event.outcome)) errors.push("final outcome is invalid");
    if (!Array.isArray(event.changedFiles) || !Array.isArray(event.remaining)) errors.push("final requires changedFiles and remaining arrays");
  }
  return errors;
}

export function readJsonl(file) {
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, "utf8").split("\n").filter(Boolean).map((line, index) => { try { return JSON.parse(line); } catch (error) { throw new Error(`${file}:${index + 1}: malformed JSONL: ${error.message}`); } });
}

export function expectedReportPath(root, runId, agent, piRunId) {
  for (const [name, value] of Object.entries({ runId, agent, piRunId })) if (!/^[A-Za-z0-9._-]+$/.test(value || "")) throw new Error(`unsafe ${name}`);
  return path.join(root, ".conductor", "runs", runId, "agents", agent, piRunId, "events.jsonl");
}

export function appendReportEvent({ root, file, event, runId, agent = process.env.PI_SUBAGENT_CHILD_AGENT, piRunId = process.env.PI_SUBAGENT_RUN_ID, heartbeatMs } = {}) {
  if (!agent || !piRunId) throw new Error("PI_SUBAGENT_CHILD_AGENT and PI_SUBAGENT_RUN_ID are required");
  const expected = expectedReportPath(root, runId, agent, piRunId);
  if (path.resolve(file) !== path.resolve(expected)) throw new Error(`report path must be ${expected}`);
  const errors = validateReportEvent(event); if (errors.length) throw new Error(errors.join("; "));
  file = safeTarget(root, path.relative(path.resolve(root), path.resolve(file)).split(path.sep).join("/"));
  fs.mkdirSync(path.dirname(file), { recursive: true });
  // Fail closed on concurrent/stale locks; never guess that another writer is dead.
  const lock = `${file}.lock`; const lockFd = fs.openSync(lock, "wx", 0o600);
  try {
  const prior = validatedHistory(file);
  const expectedSeq = prior.length + 1;
  if (event.seq !== expectedSeq) throw new Error(`event seq must be ${expectedSeq}`);
  if (prior.some((x) => x.kind === "final")) throw new Error("cannot append after final event");
  if (prior.length && Date.parse(event.at) < Date.parse(prior.at(-1).at)) throw new Error("event timestamps must be monotonic");
  // A missed heartbeat is telemetry, not a rejection: late events (including a late
  // final) always append so a timeout never locks the report or loses completed work.
  if (heartbeatMs && prior.length && Date.parse(event.at) - Date.parse(prior.at(-1).at) > heartbeatMs) { event = { ...event, heartbeatMissed: true, heartbeatMissedByMs: Date.parse(event.at) - Date.parse(prior.at(-1).at) }; }
  for (const fileName of [...(event.files || []), ...(event.changedFiles || [])]) normalizeRelative(fileName);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const before = fs.existsSync(file) ? fs.statSync(file) : null;
  const line = Buffer.from(`${JSON.stringify(event)}\n`);
  const fd = fs.openSync(file, fs.constants.O_CREAT | fs.constants.O_RDWR | fs.constants.O_APPEND | fs.constants.O_NOFOLLOW, 0o600);
  let stat;
  try {
    stat = fs.fstatSync(fd);
    if (before && (before.dev !== stat.dev || before.ino !== stat.ino || before.size !== stat.size)) throw new Error("report identity changed");
    const offset = stat.size;
    fs.writeFileSync(fd, line); fs.fsyncSync(fd);
    const reread = Buffer.alloc(line.length);
    if (fs.readSync(fd, reread, 0, line.length, offset) !== line.length || !reread.equals(line) || fs.fstatSync(fd).size !== offset + line.length) throw new Error("report append postcondition failed");
    const current = fs.statSync(file);
    if (current.dev !== stat.dev || current.ino !== stat.ino || current.size !== offset + line.length) throw new Error("report identity changed after append");
  } finally { fs.closeSync(fd); }
  // Reuse parsed objects but digest disk bytes on every call, including same-size rewrites.
  const events = [...prior, structuredClone(event)];
  histories.set(file, { stamp: stamp(fs.statSync(file, { bigint: true })), sha256: hash(fs.readFileSync(file)), events });
  if (histories.size > 128) histories.delete(histories.keys().next().value);
  return event;
  } finally { fs.closeSync(lockFd); fs.unlinkSync(lock); }
}
