#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { validateMap, resolveOwnership, deriveWaves } from "../extension/conductor-ownership.mjs";
import { readState, mutateState, prospectiveState, atomicWrite, validateActiveAgreement, refreshActiveMarker } from "./lib/state.mjs";
import { assertPlanAgreement, assertWriterEvidence } from "./lib/gates.mjs";
import { checkMapReuse, buildContextPacket } from "./lib/context.mjs";
import { inspectRuntime, processBackend } from "./lib/rpc.mjs";
import { Engine } from "./lib/engine.mjs";
import { appendReportEvent, readJsonl } from "./lib/report.mjs";
import { runVerificationPlan, consumeReview } from "./lib/verify.mjs";
import { appendTelemetry, soak, trend, telemetryFromEvents, validateTelemetry } from "./lib/telemetry.mjs";
import { validateKit, inspectDrift, installProject } from "./lib/install-manifest.mjs";

const args = process.argv.slice(2); const command = args.shift();
function option(name, fallback) { const flag = `--${name}`; const index = args.indexOf(flag); if (index < 0) return fallback; if (index === args.length - 1 || args[index + 1].startsWith("--")) return true; return args[index + 1]; }
function has(name) { return args.includes(`--${name}`); }
function root() { return path.resolve(option("root", process.cwd())); }
function jsonFile(file) { return JSON.parse(fs.readFileSync(file, "utf8")); }
function hashFile(file) { return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex"); }
function output(value) { process.stdout.write(`${typeof value === "string" ? value : JSON.stringify(value, null, 2)}\n`); }
function fail(message, code = 1) { process.stderr.write(`conductor: ${message}\n`); process.exitCode = code; }
function isHelp(value) { return ["help", "--help", "-h"].includes(value); }
const HELP = {
  state: "state validate|show|patch|transition --expected-revision N --expected-status S [--patch file] [--to status]",
  map: "map validate|resolve|waves|reuse|promote [--inventory] [--candidate file] [--refresh] [--future file]",
  context: "context packet --section <id> --pi-run-id <id> [--max-bytes N] [--output file]",
  engine: "engine status|map|build|recover [--task text] [--timeout-ms N] [--map-ms N] [--stall-ms N] [--grace-ms N] [--max-concurrency N] [--relaunch]",
  preflight: "preflight --wave <id> [--activate] [--phase build]",
  active: "active validate|refresh|clear --expected-run-id <id> [--expected-revision N]",
  report: "report append --file <path> --run-id <id> --event <json>",
  verify: "verify run --plan <file> [--output file]",
  review: "review consume --verdict <file> [--current file]",
  telemetry: "telemetry append|derive --file runs.jsonl [--record file]",
};

function inventory(projectRoot) {
  const git = spawnSync("git", ["ls-files", "--cached", "--others", "--exclude-standard", "-z"], { cwd: projectRoot, encoding: "utf8" });
  if (git.status === 0) return git.stdout.split("\0").filter(Boolean).sort();
  const found = []; const visit = (directory) => { for (const entry of fs.readdirSync(directory, { withFileTypes: true })) { if ([".git", "node_modules", ".conductor", ".vnodes"].includes(entry.name)) continue; const absolute = path.join(directory, entry.name); if (entry.isDirectory()) visit(absolute); else if (entry.isFile()) found.push(path.relative(projectRoot, absolute).replaceAll(path.sep, "/")); } }; visit(projectRoot); return found.sort();
}
function ignored(map, file) { return (map.ignored || []).some((glob) => { try { return resolveOwnership({ rules: [{ owner: "ignored", glob }] }, file).kind === "owned"; } catch { return false; } }); }
function availableAgents(projectRoot) { const directory = path.join(projectRoot, ".pi", "agents"); return fs.existsSync(directory) ? fs.readdirSync(directory).filter((x) => x.endsWith(".md")).map((x) => path.basename(x, ".md")) : []; }
// Promotion gates live in lib/gates.mjs so the coordinator CLI and the execution
// engine enforce identical evidence requirements. Re-exported for callers/tests.
export { assertPlanAgreement, assertWriterEvidence, assertBuildSettled } from "./lib/gates.mjs";

async function main() {
  if (!command || ["help", "--help", "-h"].includes(command)) return output(`Conductor CLI ${fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../VERSION"), "utf8").trim()}\ncommands: version, state, map, context, preflight, active, repair-active, engine, report, verify, review, telemetry, soak, trend, doctor, install, update`);
  if (command === "version") return output(fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../VERSION"), "utf8").trim());
  if (command === "state") {
    if (isHelp(args[0])) return output(HELP.state);
    const action = args[0] || "validate"; const file = path.resolve(root(), option("file", ".conductor-state.md"));
    if (action === "validate" || action === "show") { const state = readState(file); return output(action === "show" ? state.state : { ok: true, revision: state.state.revision, status: state.state.run.status, digest: state.digest }); }
    if (action === "transition" || action === "patch") {
      const patch = option("patch") ? jsonFile(path.resolve(option("patch"))) : {}; const expectedRevision = Number(option("expected-revision")); const expectedStatus = option("expected-status"); const toStatus = action === "transition" ? option("to") : undefined;
      const current = readState(file).state;
      const projectRoot = root(); const prospective = prospectiveState(current, patch, toStatus);
      if (toStatus === "map-done" || toStatus === "plan-done") {
        const nextMap = prospective.map; const mapFile = path.resolve(projectRoot, nextMap.path || ".conductor/section-map.json");
        if (!fs.existsSync(mapFile)) throw new Error(`${toStatus} requires an existing map`);
        const map = jsonFile(mapFile); const candidates = inventory(projectRoot).filter((x) => !ignored(map, x)); const check = validateMap(map, { candidates, availableAgents: availableAgents(projectRoot) });
        if (!check.ok) throw new Error(`${toStatus} map gate failed: ${check.errors.join("; ")}`);
        if (!nextMap.sha256 || nextMap.sha256 !== hashFile(mapFile) || !nextMap.validatedAt) throw new Error(`${toStatus} requires the validated map digest and timestamp in state`);
        if (toStatus === "plan-done") {
          if (!Object.keys(prospective.sections).length || !prospective.waves.length) throw new Error("plan-done requires sections and waves");
          const future = Object.entries(prospective.sections).flatMap(([owner, section]) => section.workOrder.declaredFiles.map((file) => ({ path: file, owner })));
          const futureCheck = validateMap(map, { future }); if (!futureCheck.ok) throw new Error(`plan future-path gate failed: ${futureCheck.errors.join("; ")}`); assertPlanAgreement(prospective, map);
        }
      }
      if (["build-done", "verify-done"].includes(toStatus)) {
        if (fs.existsSync(path.join(projectRoot, ".conductor", "active.json"))) throw new Error(`${toStatus} requires settled active state`); const map = jsonFile(path.resolve(projectRoot, prospective.map.path)); assertPlanAgreement(prospective, map); assertWriterEvidence(prospective, projectRoot);
      }
      if (toStatus === "verify-done") {
        if (prospective.verification.mechanical.status !== "passed" || prospective.verification.mechanical.checks.some((check) => check.required !== false && check.status !== "passed") || prospective.verification.review.status !== "accepted" || prospective.verification.verdict !== "accepted") throw new Error("verify-done requires all required mechanical checks passed and accepted review");
      }
      if (toStatus === "complete" && (prospective.telemetry.status !== "appended" || prospective.run.outcome !== "accepted" || !prospective.report)) throw new Error("complete requires appended telemetry, accepted outcome, and final report");
      const result = mutateState(file, { expectedRevision, expectedStatus, patch, toStatus }); return output({ ok: true, revision: result.state.revision, status: result.state.run.status, digest: result.digest });
    }
    throw new Error(`unknown state action '${action}'`);
  }
  if (command === "map") {
    if (isHelp(args[0])) return output(HELP.map);
    const action = args[0] || "validate"; const projectRoot = root(); const file = path.resolve(projectRoot, option("file", ".conductor/section-map.json"));
    if (action === "validate") { const map = jsonFile(file); const candidates = has("inventory") ? inventory(projectRoot).filter((x) => !ignored(map, x)) : []; const future = option("future") ? jsonFile(path.resolve(option("future"))) : []; const result = validateMap(map, { candidates, future, availableAgents: availableAgents(projectRoot) }); output({ ...result, candidates: candidates.length }); if (!result.ok) process.exitCode = 1; return; }
    if (action === "resolve") { const map = jsonFile(file); return output(resolveOwnership(map, option("path"))); }
    if (action === "waves") return output(deriveWaves(jsonFile(file)));
    // Validated reuse: a cache hit still revalidates map, inventory, agents, and
    // future paths. It never bypasses an ownership or acceptance check.
    if (action === "reuse") {
      const future = option("future") ? jsonFile(path.resolve(option("future"))) : [];
      const result = checkMapReuse({ root: projectRoot, mapPath: path.relative(projectRoot, file).split(path.sep).join("/"), future, refresh: has("refresh") });
      output(result); if (!result.ok) process.exitCode = 1;
      return;
    }
    if (action === "promote") {
      const candidate = path.resolve(option("candidate")); const map = jsonFile(candidate); const candidates = inventory(projectRoot).filter((x) => !ignored(map, x)); const result = validateMap(map, { candidates, availableAgents: availableAgents(projectRoot) }); if (!result.ok) throw new Error(result.errors.join("\n")); atomicWrite(file, fs.readFileSync(candidate)); return output({ ok: true, path: file, sha256: hashFile(file), waves: result.waves });
    }
    throw new Error(`unknown map action '${action}'`);
  }
  if (command === "context") {
    if (isHelp(args[0]) || !args[0]) return output(HELP.context);
    const action = args[0]; const projectRoot = root(); const state = readState(path.join(projectRoot, ".conductor-state.md")).state;
    if (action !== "packet") throw new Error(`unknown context action '${action}'. ${HELP.context}`);
    const sectionId = option("section"); const piRunId = option("pi-run-id");
    if (!sectionId || !piRunId) throw new Error("context packet requires --section and --pi-run-id");
    const packet = buildContextPacket({ root: projectRoot, state, sectionId, piRunId, maxBytes: Number(option("max-bytes", 48000)) || 48000 });
    if (option("output")) atomicWrite(path.resolve(option("output")), `${JSON.stringify(packet, null, 2)}\n`);
    return output({ ok: true, bytes: Buffer.byteLength(JSON.stringify(packet)), sectionId, piRunId, contracts: packet.contracts.length, source: packet.source.length, checkpoints: packet.checkpoints.length, output: option("output") ? path.resolve(option("output")) : null });
  }
  if (command === "engine") {
    if (isHelp(args[0])) return output(HELP.engine);
    const action = args[0] || "status"; const projectRoot = root();
    const engine = new Engine({
      root: projectRoot, kitRoot: option("kit-root"), backend: processBackend,
      model: option("model") || undefined, thinking: option("thinking") || undefined,
    });
    if (action === "status") return output(engine.status());
    if (action === "map") {
      const budgets = {};
      for (const [flag, key] of [["map-ms", "mapMs"], ["stall-ms", "stallMs"], ["grace-ms", "graceMs"], ["heartbeat-ms", "heartbeatMs"], ["explore-ms", "exploreMs"], ["build-ms", "buildMs"], ["verify-ms", "verifyMs"]]) {
        if (option(flag) !== undefined && option(flag) !== true) budgets[key] = Number(option(flag));
      }
      const result = await engine.map({
        task: option("task") || undefined,
        timeoutMs: option("timeout-ms") ? Number(option("timeout-ms")) : undefined,
        budgets: Object.keys(budgets).length ? budgets : undefined,
      });
      output(result); if (!result.ok) process.exitCode = 1;
      return;
    }
    if (action === "build") {
      const result = await engine.build({ maxConcurrency: option("max-concurrency") ? Number(option("max-concurrency")) : undefined });
      output(result); if (!result.ok) process.exitCode = 1;
      return;
    }
    if (action === "recover") {
      const result = await engine.recover({ relaunch: has("relaunch"), maxConcurrency: option("max-concurrency") ? Number(option("max-concurrency")) : undefined });
      output(result); if (result.relaunched && !result.ok) process.exitCode = 1;
      return;
    }
    throw new Error(`unknown engine action '${action}'. ${HELP.engine}`);
  }
  if (command === "preflight") {
    const projectRoot = root(); const state = readState(path.join(projectRoot, ".conductor-state.md")).state; const mapFile = path.resolve(projectRoot, state.map.path); const map = jsonFile(mapFile);
    if (state.map.sha256 && hashFile(mapFile) !== state.map.sha256) throw new Error("map digest does not match state");
    const mapCheck = validateMap(map, { candidates: inventory(projectRoot).filter((x) => !ignored(map, x)), availableAgents: availableAgents(projectRoot) }); if (!mapCheck.ok) throw new Error(mapCheck.errors.join("\n"));
    const waveId = option("wave"); const wave = state.waves.find((x) => x.id === waveId); if (!wave) throw new Error(`unknown wave '${waveId}'`);
    if (fs.existsSync(path.join(projectRoot, ".conductor", "active.json"))) throw new Error("stale active.json exists; settle or run repair-active explicitly");
    const executions = []; const declared = new Set();
    for (const sectionId of wave.sections) {
      const section = state.sections[sectionId]; if (!section) throw new Error(`wave contains unknown section '${sectionId}'`);
      if (section.status !== "in-progress") throw new Error(`section '${sectionId}' must be in-progress before activation`);
      for (const dep of section.dependsOn) if (!["done", "skipped"].includes(state.sections[dep]?.status)) throw new Error(`dependency '${dep}' for '${sectionId}' is incomplete`);
      for (const file of section.workOrder.declaredFiles) { if (declared.has(file)) throw new Error(`declared file '${file}' appears in multiple executions`); declared.add(file); const resolution = resolveOwnership(map, file); if (resolution.kind !== "owned" || resolution.owner !== sectionId) throw new Error(`'${file}' resolves to '${resolution.kind === "owned" ? resolution.owner : resolution.kind}', expected '${sectionId}'`); }
      executions.push({ section: sectionId, agent: section.agent, declaredFiles: section.workOrder.declaredFiles });
    }
    const active = { schemaVersion: 1, runId: state.run.id, stateRevision: state.revision, waveId, phase: option("phase", "build"), executions };
    const agreement = validateActiveAgreement(state, active); if (agreement.length) throw new Error(agreement.join("\n"));
    if (has("activate")) { const activeFile = path.join(projectRoot, ".conductor", "active.json"); atomicWrite(activeFile, `${JSON.stringify(active, null, 2)}\n`); const reread = jsonFile(activeFile); const errors = validateActiveAgreement(state, reread); if (errors.length) throw new Error(`active postcondition failed: ${errors.join("; ")}`); }
    return output({ ok: true, active, activated: has("activate") });
  }
  if (command === "active") {
    const action = args[0] || "validate", projectRoot = root(), activeFile = path.join(projectRoot, ".conductor", "active.json");
    if (!fs.existsSync(activeFile)) throw new Error("active.json is absent");
    if (action === "refresh") { const refreshed = refreshActiveMarker(projectRoot, { expectedRunId: option("expected-run-id") }); return output({ ok: true, active: refreshed }); }
    const state = readState(path.join(projectRoot, ".conductor-state.md")).state; const active = jsonFile(activeFile); const errors = validateActiveAgreement(state, active); if (errors.length) throw new Error(errors.join("; "));
    if (action === "validate") return output({ ok: true, active });
    if (action === "clear") {
      if (option("expected-run-id") !== active.runId || Number(option("expected-revision")) !== active.stateRevision) throw new Error("active clear expected run/revision conflict"); fs.unlinkSync(activeFile); const fd = fs.openSync(path.dirname(activeFile), "r"); try { fs.fsyncSync(fd); } finally { fs.closeSync(fd); } if (fs.existsSync(activeFile)) throw new Error("active clear postcondition failed"); return output({ ok: true, cleared: true, runId: active.runId, stateRevision: active.stateRevision });
    }
    throw new Error(`unknown active action '${action}'`);
  }
  if (command === "repair-active") {
    if (!has("from-state")) throw new Error("repair-active requires --from-state"); const projectRoot = root(); const state = readState(path.join(projectRoot, ".conductor-state.md")).state; const inProgress = Object.entries(state.sections).filter(([, section]) => section.status === "in-progress").map(([id]) => id).sort();
    const matches = state.waves.filter((wave) => JSON.stringify([...wave.sections].sort()) === JSON.stringify(inProgress)); if (matches.length !== 1) throw new Error(`cannot derive one active wave from in-progress sections: ${inProgress.join(", ") || "none"}`);
    const activeFile = path.join(projectRoot, ".conductor", "active.json"); fs.rmSync(activeFile, { force: true }); const result = spawnSync(process.execPath, [fileURLToPath(import.meta.url), "preflight", "--root", projectRoot, "--wave", matches[0].id, "--phase", option("phase", "build"), "--activate"], { encoding: "utf8" }); if (result.status !== 0) throw new Error(`active repair failed: ${result.stderr.trim()}`); return output({ ok: true, waveId: matches[0].id, active: jsonFile(activeFile) });
  }
  if (command === "report") {
    if (args[0] !== "append") throw new Error("report action must be append"); const event = option("event-file") ? jsonFile(path.resolve(option("event-file"))) : JSON.parse(option("event")); const projectRoot = root(); const result = appendReportEvent({ root: projectRoot, file: path.resolve(option("file")), event, runId: option("run-id"), heartbeatMs: Number(option("heartbeat-ms", 0)) || undefined }); return output({ ok: true, event: result });
  }
  if (command === "verify") {
    if (args[0] !== "run") throw new Error("verify action must be run"); const projectRoot = root(); const result = await runVerificationPlan(jsonFile(path.resolve(option("plan"))), { root: projectRoot }); if (option("output")) atomicWrite(path.resolve(option("output")), `${JSON.stringify(result, null, 2)}\n`); output(result); if (result.status !== "passed") process.exitCode = 1; return;
  }
  if (command === "review") {
    if (args[0] !== "consume") throw new Error("review action must be consume"); const current = option("current") ? jsonFile(path.resolve(option("current"))) : { status: "pending", attempts: 0, lastSnapshot: null }; return output(consumeReview(current, jsonFile(path.resolve(option("verdict")))));
  }
  if (command === "telemetry") {
    const action = args[0]; const projectRoot = root(); const file = path.resolve(projectRoot, option("file", ".conductor/runs.jsonl"));
    if (action === "append") return output(appendTelemetry(file, jsonFile(path.resolve(option("record")))));
    if (action === "derive") { const events = readJsonl(path.resolve(option("events"))); const record = telemetryFromEvents({ runId: option("run-id"), conductorVersion: option("version", fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../VERSION"), "utf8").trim()), task: option("task", ""), startedAt: option("started-at"), endedAt: option("ended-at"), events }); const errors = validateTelemetry(record); if (errors.length) throw new Error(`derived telemetry is invalid: ${errors.join("; ")}`); if (option("output")) atomicWrite(path.resolve(option("output")), `${JSON.stringify(record, null, 2)}\n`); return output(record); }
    throw new Error("telemetry action must be append or derive");
  }
  if (command === "soak" || command === "trend") { const records = readJsonl(path.resolve(root(), option("file", ".conductor/runs.jsonl"))); const result = command === "soak" ? soak(records, Number(option("n", 10))) : trend(records); output(result); if (command === "soak" ? !result.passed : result.alert) process.exitCode = 1; return; }
  if (command === "doctor") {
    const kitRoot = path.resolve(option("kit-root", path.join(path.dirname(fileURLToPath(import.meta.url)), ".."))); const kit = validateKit(kitRoot); const projectRoot = option("root") ? root() : null;
    // Kit validity and execution readiness are different claims. A kit can be
    // perfectly valid while this machine cannot run a managed child; doctor must
    // never imply enforcement it has not established.
    const execution = inspectRuntime({ root: projectRoot || undefined, kitRoot });
    const result = { kit, execution, node: process.version, minimumNode: "22.19.0", ready: kit.ok && execution.ok };
    if (projectRoot) { const manifest = path.join(projectRoot, ".conductor", "install.json"); result.project = { manifest: fs.existsSync(manifest), drift: fs.existsSync(manifest) ? inspectDrift(kitRoot, projectRoot).drift : ["not installed"] }; }
    // A valid kit on a machine without pi is still a valid kit: report readiness
    // honestly, and gate it only when the caller asks. `engine build` fails closed
    // on its own, so an unsupported run can never start silently.
    output(result); if (!kit.ok || result.project?.drift.length || (has("require-execution") && !execution.ok)) process.exitCode = 1; return;
  }
  if (command === "install" || command === "update") { const kitRoot = path.resolve(option("source", path.join(path.dirname(fileURLToPath(import.meta.url)), ".."))); return output(installProject({ source: kitRoot, root: root(), mode: command, force: has("force") })); }
  throw new Error(`unknown command '${command}'`);
}

function samePath(a, b) { try { return fs.realpathSync(a) === fs.realpathSync(b); } catch { return path.resolve(a) === path.resolve(b); } }
if (process.argv[1] && samePath(process.argv[1], fileURLToPath(import.meta.url))) main().catch((error) => fail(error.stack || error.message));
