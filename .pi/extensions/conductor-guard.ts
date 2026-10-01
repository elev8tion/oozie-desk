// Conductor 2.0 guard: fail-closed write/edit policy plus bounded structured tools.
// write/edit interception is defense in depth. Structured conductor_command replaces
// raw bash for managed agents; OS-level sandboxing is still required against arbitrary
// subprocesses exposed by unrelated extensions.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { StringEnum } from "@earendil-works/pi-ai";
import { Type } from "typebox";
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { canonicalTarget, resolveActiveExecution, resolveOwnership, validateMap, assertExecutionAuthorization } from "./conductor-ownership.mjs";

export const CONDUCTOR_GUARD_VERSION = "2.1.1";
const MAX_BYTES = 128 * 1024;
const DEFAULT_AGENTS = new Set(["cartographer", "reviewer", "config", "data", "logic", "routes", "interface", "tests", "docs"]);
const digest = (file: string) => crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");

function bounded(text: string) { return text.length <= MAX_BYTES ? { text, truncated: false } : { text: text.slice(0, MAX_BYTES), truncated: true }; }
function safePaths(values: string[] = []) { return values.map((raw) => { const value = raw.replace(/^@/, "").replaceAll("\\", "/"); if (!value || value.startsWith("/") || /^[A-Za-z]:\//.test(value) || value.split("/").includes("..")) throw new Error(`unsafe repository-relative path: ${raw}`); return value; }); }
function loadJson(file: string) { return JSON.parse(fs.readFileSync(file, "utf8")); }
function parseMachineState(file: string) {
  const text = fs.readFileSync(file, "utf8"); const begin = "<!-- CONDUCTOR:STATE:BEGIN -->"; const end = "<!-- CONDUCTOR:STATE:END -->"; const a = text.indexOf(begin), b = text.indexOf(end);
  if (a < 0 || b <= a) throw new Error("invalid state markers"); const body = text.slice(a + begin.length, b).trim(); const match = body.match(/^```json\s*\n([\s\S]*?)\n```$/); if (!match) throw new Error("invalid state JSON block"); return JSON.parse(match[1]);
}
function ownReport(rel: string, runId: string, agent: string, piRunId: string) { return Boolean(runId && piRunId) && new RegExp(`^\\.conductor/runs/${runId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/agents/${agent.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/${piRunId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/`).test(rel); }

export default function conductorGuard(pi: ExtensionAPI) {
  const managed = process.env.CONDUCTOR_RPC === "1";
  const allowed = ["read", "write", "edit", "grep", "find", "ls", "conductor_guard_status", "conductor_evidence", "conductor_command", "conductor_report"];
  function authorize(root: string) {
    const state = parseMachineState(path.join(root, ".conductor-state.md"));
    const active = loadJson(path.join(root, ".conductor", "active.json"));
    const execution = resolveActiveExecution(active, process.env.PI_SUBAGENT_CHILD_AGENT || "", process.env.PI_SUBAGENT_RUN_ID || "");
    assertExecutionAuthorization(state, active, execution);
    if (managed && execution.token !== process.env.CONDUCTOR_EXECUTION_TOKEN) throw new Error("execution capability token mismatch");
    // A map child is allowed to run against an unvalidated starter map; writers are not.
    if (active.phase !== "map") {
      const target = canonicalTarget(root, state.map.path); if (!target.inside || digest(target.absolute) !== state.map.sha256) throw new Error("map digest mismatch");
      const validation = validateMap(loadJson(target.absolute)); if (!validation.ok) throw new Error(validation.errors.join("; "));
    }
    return { state, active, execution };
  }
  if (managed) {
    pi.on("session_start", () => {
      const next = pi.getActiveTools().filter((name) => allowed.includes(name));
      if (next.length) pi.setActiveTools(next);
    });
    pi.on("before_agent_start", (_event, ctx) => { authorize(ctx.cwd); });
    pi.registerCommand("conductor-probe", { description: "No-LLM guard capability handshake", handler: async (nonce, ctx) => {
      let ready = false, reason = "unbound";
      try { authorize(ctx.cwd); ready = true; reason = "bound"; } catch (error) { reason = (error as Error).message; }
      ctx.ui.notify(JSON.stringify({ type: "conductor_ready", nonce, version: CONDUCTOR_GUARD_VERSION, ready, reason, agent: process.env.PI_SUBAGENT_CHILD_AGENT, piRunId: process.env.PI_SUBAGENT_RUN_ID, tools: pi.getActiveTools().sort(), sessionId: ctx.sessionManager.getSessionId(), sessionFile: ctx.sessionManager.getSessionFile() }), "info");
    } });
  }
  pi.registerTool({
    name: "conductor_guard_status", label: "Conductor guard status", description: "Return the loaded Conductor guard version and child identity.", parameters: Type.Object({}),
    async execute() { return { content: [{ type: "text", text: JSON.stringify({ loaded: true, version: CONDUCTOR_GUARD_VERSION, agent: process.env.PI_SUBAGENT_CHILD_AGENT || null }) }], details: { version: CONDUCTOR_GUARD_VERSION } }; },
  });

  pi.registerTool({
    name: "conductor_evidence", label: "Conductor evidence", description: "Collect bounded read-only Git evidence without a shell.",
    parameters: Type.Object({ action: StringEnum(["git_status", "git_diff", "git_diff_names", "git_ls_files"] as const), staged: Type.Optional(Type.Boolean()), paths: Type.Optional(Type.Array(Type.String({ minLength: 1, maxLength: 500 }), { maxItems: 100 })) }),
    async execute(_id, params, signal, _onUpdate, ctx) {
      const paths = safePaths(params.paths || []); const common = ["--no-pager", "--literal-pathspecs", "-c", "core.fsmonitor=false", "--no-optional-locks"];
      let command: string[];
      if (params.action === "git_status") command = [...common, "status", "--short", "--untracked-files=all", "--", ...paths];
      else if (params.action === "git_ls_files") command = [...common, "ls-files", "--cached", "--others", "--exclude-standard", "--", ...paths];
      else command = [...common, "diff", ...(params.action === "git_diff_names" ? ["--name-status"] : []), "--no-ext-diff", "--no-textconv", ...(params.staged ? ["--cached"] : []), "--", ...paths];
      const result = await pi.exec("git", command, { cwd: ctx.cwd, signal, timeout: 10000 });
      if (result.code !== 0) {
        const stderr = result.stderr || `git exited ${result.code}`;
        if (/not a git repository/i.test(stderr)) return { content: [{ type: "text", text: "(not a git repository)" }], details: { action: params.action, git: false, truncated: false, exitCode: result.code } };
        throw new Error(stderr);
      }
      const value = bounded(result.stdout || "(no output)");
      return { content: [{ type: "text", text: value.text }], details: { action: params.action, truncated: value.truncated, exitCode: result.code } };
    },
  });

  pi.registerTool({
    name: "conductor_report", label: "Conductor checkpoint", description: "Append one identity-bound, schema-validated checkpoint/deviation/final JSON event.",
    parameters: Type.Object({ runId: Type.String({ minLength: 1, maxLength: 100 }), event: Type.String({ minLength: 2, maxLength: 20000 }) }),
    async execute(_id, params, signal, _onUpdate, ctx) {
      const agent = (process.env.PI_SUBAGENT_CHILD_AGENT || "").trim(); const piRunId = (process.env.PI_SUBAGENT_RUN_ID || "").trim(); if (!agent || !piRunId) throw new Error("conductor_report requires child agent/run identity");
      let event: any; try { event = JSON.parse(params.event); } catch (error) { throw new Error(`event must be JSON: ${(error as Error).message}`); }
      const runtime = path.join(ctx.cwd, ".conductor", "runtime", "scripts", "conductor.mjs"); const report = path.join(ctx.cwd, ".conductor", "runs", params.runId, "agents", agent, piRunId, "events.jsonl"); const state = parseMachineState(path.join(ctx.cwd, ".conductor-state.md")); if (state.run.id !== params.runId) throw new Error("report runId does not match active state");
      if (managed) authorize(ctx.cwd);
      const heartbeat = Object.values(state.sections || {}).find((value: any) => value.agent === agent)?.workOrder?.budgets?.heartbeatMs;
      const result = await pi.exec(process.execPath, [runtime, "report", "append", "--root", ctx.cwd, "--file", report, "--run-id", params.runId, "--event", JSON.stringify(event), ...(heartbeat ? ["--heartbeat-ms", String(heartbeat)] : [])], { cwd: ctx.cwd, signal, timeout: 10000 });
      if (result.code !== 0) throw new Error(result.stderr || `report helper exited ${result.code}`); return { content: [{ type: "text", text: `checkpoint ${event.seq} appended` }], details: { runId: params.runId, seq: event.seq, kind: event.kind, path: report } };
    },
  });

  pi.registerTool({
    name: "conductor_command", label: "Conductor command", description: "Run one exact shell-free command ID declared in the active work order.", parameters: Type.Object({ commandId: Type.String({ minLength: 1, maxLength: 100 }) }),
    async execute(_id, params, signal, _onUpdate, ctx) {
      const agent = (process.env.PI_SUBAGENT_CHILD_AGENT || "").trim(); const piRunId = (process.env.PI_SUBAGENT_RUN_ID || "").trim(); if (!agent || !piRunId) throw new Error("conductor_command is child-only and requires child/run identity");
      const state = parseMachineState(path.join(ctx.cwd, ".conductor-state.md")); const active = loadJson(path.join(ctx.cwd, ".conductor", "active.json")); const execution = resolveActiveExecution(active, agent, piRunId); assertExecutionAuthorization(state, active, execution); authorize(ctx.cwd);
      const section = state.sections?.[execution.section] as any; if (!section || section.status !== "in-progress" || section.agent !== agent) throw new Error("active work-order agreement failed");
      const command = section.workOrder?.commands?.find((item: any) => item.id === params.commandId); if (!command || !Array.isArray(command.argv) || !command.argv.length) throw new Error(`command '${params.commandId}' is not declared for ${agent}`);
      const cwdTarget = canonicalTarget(ctx.cwd, command.cwd || "."); if (!cwdTarget.inside && command.cwd !== ".") throw new Error("command cwd escapes project"); const cwd = command.cwd === "." ? fs.realpathSync(ctx.cwd) : cwdTarget.absolute;
      const result = await pi.exec(command.argv[0], command.argv.slice(1), { cwd, signal, timeout: Math.min(command.timeoutMs || 120000, 600000) }); const value = bounded(`${result.stdout || ""}${result.stderr ? `\n${result.stderr}` : ""}`);
      if (result.code !== 0) throw new Error(`command '${params.commandId}' exited ${result.code}\n${value.text}`); return { content: [{ type: "text", text: value.text || "command passed" }], details: { commandId: params.commandId, exitCode: result.code, truncated: value.truncated } };
    },
  });

  pi.on("tool_call", async (event, ctx) => {
    if (managed) { if (!allowed.includes(event.toolName)) return { block: true, reason: "conductor: tool not allowed" }; try { authorize(ctx.cwd); } catch (error) { return { block: true, reason: `conductor: ${(error as Error).message}` }; } }
    if (event.toolName !== "write" && event.toolName !== "edit") return;
    const root = ctx.cwd; const agent = (process.env.PI_SUBAGENT_CHILD_AGENT || "").trim(); const piRunId = (process.env.PI_SUBAGENT_RUN_ID || "").trim(); const activeFile = path.join(root, ".conductor", "active.json");
    const activePresent = fs.existsSync(activeFile); if (!agent && !activePresent) return;
    if (agent && !activePresent && !DEFAULT_AGENTS.has(agent)) {
      try {
        const state = parseMachineState(path.join(root, ".conductor-state.md")); let known = Object.values(state.sections || {}).some((section: any) => section.agent === agent);
        if (!known) { const map = loadJson(path.resolve(root, state.map.path)); known = (map.sections || []).some((section: any) => section.agent === agent); }
        if (!known) return;
      } catch { return; } // unrelated non-Conductor children remain unaffected when no run is active
    }
    const raw = (event.input as { path?: string }).path || ""; if (!raw) return { block: true, reason: "conductor: mutation path is required." };
    let target; try { target = canonicalTarget(root, raw); } catch (error) { return { block: true, reason: `conductor: cannot canonicalize mutation path: ${(error as Error).message}` }; }
    if (!target.inside) return { block: true, reason: `conductor: writes outside the project are blocked (${target.absolute}).` }; const rel = target.relative;
    if (agent && rel.startsWith(".conductor/runs/")) {
      try { const reportState = parseMachineState(path.join(root, ".conductor-state.md")); if (ownReport(rel, reportState.run.id, agent, piRunId)) { if (managed) { authorize(root); if (rel.endsWith("/events.jsonl")) return { block: true, reason: "conductor: use conductor_report for semantic events" }; } return; } }
      catch (error) { return { block: true, reason: `conductor: report identity validation failed: ${(error as Error).message}` }; }
    }
    const isControl = rel === ".conductor-state.md" || rel === ".conductor" || rel.startsWith(".conductor/");
    if (agent && isControl) return { block: true, reason: `conductor: child '${agent}' may write only its identity-bound report directory, not ${rel}.` };
    if (!agent && isControl) return; // coordinator is the only shared-state writer
    if (!activePresent) return { block: true, reason: "conductor: child source mutation blocked because active.json is missing." };
    let active: any, map: any, state: any;
    try {
      active = loadJson(activeFile); if (active.schemaVersion !== 1 || !["build", "verify", "repair", "map"].includes(active.phase) || !Array.isArray(active.executions) || !active.executions.length) throw new Error("invalid active schema");
      state = parseMachineState(path.join(root, ".conductor-state.md")); if (active.runId !== state.run.id || (active.authorization !== "snapshot" && (active.stateRevision !== state.revision || !state.waves?.some((wave: any) => wave.id === active.waveId)))) throw new Error("active/state mismatch");
      if (active.authorization === "snapshot") authorize(root);
      for (const item of (active.authorization === "snapshot" ? [] : active.executions)) { const section = state.sections?.[item.section]; if (!section || section.status !== "in-progress" || section.agent !== item.agent || JSON.stringify([...(section.workOrder?.declaredFiles || [])].sort()) !== JSON.stringify([...(item.declaredFiles || [])].sort())) throw new Error(`active work-order mismatch for '${item.section}'`); const bound = [...(section.attempts || [])].at(-1)?.piRunId; if (bound && item.piRunId !== bound) throw new Error(`active run identity mismatch for '${item.section}'`); }
      const mapTarget = canonicalTarget(root, state.map.path); if (!mapTarget.inside) throw new Error("map path escapes project"); const mapFile = mapTarget.absolute; map = loadJson(mapFile); const validation = validateMap(map); if (!validation.ok) throw new Error(validation.errors.join("; ")); if (!/^[a-f0-9]{64}$/.test(state.map.sha256 || "") || digest(mapFile) !== state.map.sha256) throw new Error("map digest mismatch");
    } catch (error) { return { block: true, reason: `conductor: active run control plane is invalid; fail closed: ${(error as Error).message}` }; }
    if (!agent) return { block: true, reason: `conductor: coordinator may not edit source during an active run (${rel}).` };
    if (!piRunId) return { block: true, reason: `conductor: source mutation requires a child run identity (PI_SUBAGENT_RUN_ID).` };
    const execution = resolveActiveExecution(active, agent, piRunId); if (!execution) return { block: true, reason: `conductor: unknown, unbound, stale, or inactive child identity '${agent}'/'${piRunId}'.` };
    const resolution = resolveOwnership(map, rel); if (resolution.kind !== "owned" || resolution.owner !== execution.section) return { block: true, reason: `conductor: ${rel} resolves to '${resolution.kind === "owned" ? resolution.owner : resolution.kind}', not '${execution.section}'.` };
    if (!execution.declaredFiles.includes(rel)) return { block: true, reason: `conductor: ${rel} is section-owned but absent from this execution's declared files.` };
    return;
  });
}
