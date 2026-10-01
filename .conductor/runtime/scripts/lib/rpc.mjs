// Pi RPC backend for Conductor managed children.
//
// Replaces the missing legacy pi-subagents async controls with a direct, bounded
// JSONL RPC peer. Identity is bound in the child environment BEFORE any prompt,
// so there is no window where an unbound child can write. Readiness is probed
// through the guard's extension command, which executes without an LLM call.
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { spawn, spawnSync } from "node:child_process";

const MANAGED_TOOLS = ["read", "write", "edit", "grep", "find", "ls", "conductor_command", "conductor_report", "conductor_evidence", "conductor_guard_status"];
const MAPPER_TOOLS = ["read", "write", "grep", "find", "ls", "conductor_evidence", "conductor_report", "conductor_guard_status"];
const READ_ONLY_TOOLS = ["read", "grep", "find", "ls", "conductor_evidence", "conductor_guard_status"];

export function nonce() { return crypto.randomBytes(16).toString("hex"); }
export function mintRunId() { return `r-${new Date().toISOString().replace(/[:.]/g, "").slice(0, 15)}-${crypto.randomBytes(4).toString("hex")}`; }
export function mintToken() { return crypto.randomBytes(32).toString("hex"); }

// Strict JSONL framing: split on "\n" only and strip a trailing "\r".
// Node readline is not protocol-compliant here because U+2028/U+2029 are valid
// inside JSON strings and readline treats them as record separators.
export function attachJsonl(stream, onLine) {
  const decoder = new TextDecoder("utf8"); let buffer = "";
  const flush = (line) => { if (!line) return; onLine(line); };
  stream.on("data", (chunk) => {
    buffer += typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
    let index; while ((index = buffer.indexOf("\n")) >= 0) { const line = buffer.slice(0, index); buffer = buffer.slice(index + 1); flush(line.endsWith("\r") ? line.slice(0, -1) : line); }
  });
  stream.on("end", () => { buffer += decoder.decode(); if (buffer.endsWith("\r")) buffer = buffer.slice(0, -1); flush(buffer); buffer = ""; });
}

export function resolvePiBinary(explicit) {
  if (explicit) return explicit;
  if (process.env.CONDUCTOR_PI_BIN) return process.env.CONDUCTOR_PI_BIN;
  const found = spawnSync("pi", ["--version"], { encoding: "utf8", timeout: 20000 });
  return found.status === 0 ? "pi" : null;
}

// Children inherit the parent Pi session the same way interactive subagents do.
// --model / CONDUCTOR_MODEL override; otherwise PI_PROVIDER + PI_MODEL. There is
// no hidden catalog default — only an inherited session or an explicit override.
export function resolveSessionModel({ model, thinking, env = process.env } = {}) {
  const override = (typeof model === "string" && model.trim()) ? model.trim() : (typeof env.CONDUCTOR_MODEL === "string" && env.CONDUCTOR_MODEL.trim()) ? env.CONDUCTOR_MODEL.trim() : null;
  const provider = typeof env.PI_PROVIDER === "string" ? env.PI_PROVIDER.trim() : "";
  const piModel = typeof env.PI_MODEL === "string" ? env.PI_MODEL.trim() : "";
  let resolved = override;
  let source = override ? (model && String(model).trim() ? "flag" : "CONDUCTOR_MODEL") : "none";
  if (!resolved && piModel) {
    resolved = provider && !piModel.includes("/") ? `${provider}/${piModel}` : piModel;
    source = "parent-session";
  }
  const thinkOverride = (typeof thinking === "string" && thinking.trim()) ? thinking.trim() : null;
  const thinkEnv = env.CONDUCTOR_THINKING || env.PI_REASONING_LEVEL || env.PI_THINKING || null;
  const think = thinkOverride || (typeof thinkEnv === "string" && thinkEnv.trim()) || null;
  return { model: resolved || null, thinking: think || null, source };
}

// Build the exact argv/env for one managed child. Discovery is disabled and only
// the installed guard is loaded, so an unrelated extension cannot widen the child.
export function resolveInvocation({ root, piBinary, guardPath, agent, piRunId, token, model, thinking, readOnly = false, role = "writer", sessionDir, extraExtensions = [] }) {
  if (!piBinary) throw new Error("pi binary is not available; cannot launch a managed child");
  if (!agent || !piRunId || !token) throw new Error("managed child requires agent, piRunId, and execution token");
  if (!/^[A-Za-z0-9._-]+$/.test(piRunId)) throw new Error("unsafe piRunId");
  if (!fs.existsSync(guardPath)) throw new Error(`guard extension is missing: ${guardPath}`);
  const tools = role === "mapper" ? MAPPER_TOOLS : readOnly ? READ_ONLY_TOOLS : MANAGED_TOOLS;
  const args = ["--mode", "rpc", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-themes",
    "-e", guardPath, ...extraExtensions.flatMap((item) => ["-e", item]),
    "--tools", tools.join(","), ...(model ? ["--model", model] : []), ...(thinking ? ["--thinking", thinking] : []),
    ...(sessionDir ? ["--session-dir", sessionDir] : ["--no-session"])];
  const env = { ...process.env, PI_SUBAGENT_CHILD_AGENT: agent, PI_SUBAGENT_RUN_ID: piRunId, CONDUCTOR_RPC: "1", CONDUCTOR_EXECUTION_TOKEN: token, CONDUCTOR_PROJECT_ROOT: path.resolve(root) };
  return { command: piBinary, args, env, cwd: path.resolve(root), tools };
}

// A peer owns one RPC child process: request/response correlation, event fan-out,
// and bounded waiting. `close()` always terminates the process tree.
export function createProcessPeer({ command, args, cwd, env, stderrSink }) {
  const child = spawn(command, args, { cwd, env, shell: false, stdio: ["pipe", "pipe", "pipe"], detached: process.platform !== "win32" });
  const listeners = new Set(); const pending = new Map(); const stderr = []; const recent = [];
  let settled = false; let exitInfo = null;
  if (child.stderr) child.stderr.on("data", (chunk) => { const text = chunk.toString("utf8"); stderr.push(text); if (stderrSink) stderrSink(text); });
  const killTree = (signal) => { let killed = false; if (child.pid && process.platform !== "win32") { try { process.kill(-child.pid, signal); killed = true; } catch {} } if (!killed) { try { child.kill(signal); } catch {} } };
  const finish = (info) => { if (settled) return; settled = true; exitInfo = info; for (const [, entry] of pending) entry.reject(new Error(`rpc peer exited: ${JSON.stringify(info)}`)); pending.clear(); for (const listener of listeners) listener({ type: "__peer_exit", ...info }); };
  const dispatch = (event) => { recent.push(event); if (recent.length > 64) recent.shift(); for (const listener of listeners) listener(event); };
  child.on("error", (error) => finish({ code: null, signal: null, error: error.message }));
  child.on("close", (code, signal) => finish({ code, signal }));
  attachJsonl(child.stdout, (line) => {
    let event; try { event = JSON.parse(line); } catch { return; }
    if (event.type === "response" && event.id !== undefined && pending.has(event.id)) { const entry = pending.get(event.id); pending.delete(event.id); clearTimeout(entry.timer); entry.resolve(event); return; }
    dispatch(event);
  });
  return {
    get pid() { return child.pid; },
    get exited() { return settled; },
    get exitInfo() { return exitInfo; },
    get stderrTail() { return stderr.join("").slice(-4096); },
    onEvent(handler) { listeners.add(handler); return () => listeners.delete(handler); },
    send(command_) { if (settled || !child.stdin.writable) throw new Error("rpc peer is not writable"); child.stdin.write(`${JSON.stringify(command_)}\n`); },
    request(command_, timeoutMs = 30000) {
      const id = command_.id ?? `req-${crypto.randomBytes(6).toString("hex")}`;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => { pending.delete(id); reject(new Error(`rpc request '${command_.type}' timed out after ${timeoutMs}ms`)); }, timeoutMs);
        timer.unref?.();
        pending.set(id, { resolve, reject, timer });
        try { this.send({ ...command_, id }); } catch (error) { clearTimeout(timer); pending.delete(id); reject(error); }
      });
    },
    // Wait for a matching event. Replay recent unmatched events first so a notify
    // that arrived just before subscribe is not lost (Pi emits notify before the prompt response).
    waitEvent(predicate, timeoutMs = 30000, description = "event") {
      let cancel;
      const promise = new Promise((resolve, reject) => {
        let done = false;
        const finishWait = (event, error) => { if (done) return; done = true; clearTimeout(timer); off(); if (error) reject(error); else resolve(event); };
        const timer = setTimeout(() => finishWait(undefined, new Error(`timed out after ${timeoutMs}ms waiting for ${description}`)), timeoutMs);
        timer.unref?.();
        cancel = () => finishWait(undefined, new Error(`cancelled waiting for ${description}`));
        const off = this.onEvent((event) => {
          if (event.type === "__peer_exit") return finishWait(undefined, new Error(`rpc peer exited while waiting for ${description}`));
          let match = false; try { match = predicate(event); } catch { match = false; }
          if (match) finishWait(event);
        });
        for (const event of recent) {
          if (done) break;
          let match = false; try { match = predicate(event); } catch { match = false; }
          if (match) finishWait(event);
        }
      });
      promise.cancel = () => { try { cancel?.(); } catch { /* already settled */ } };
      return promise;
    },
    abort(timeoutMs = 10000) { if (settled) return Promise.resolve(exitInfo); return this.request({ type: "abort" }, timeoutMs).catch(() => null); },
    close(timeoutMs = 5000) {
      if (settled) return Promise.resolve(exitInfo);
      return new Promise((resolve) => {
        const done = setTimeout(() => { killTree("SIGKILL"); resolve(exitInfo); }, timeoutMs); done.unref?.();
        const off = this.onEvent((event) => { if (event.type === "__peer_exit") { clearTimeout(done); off(); resolve(exitInfo); } });
        killTree("SIGTERM");
      });
    },
  };
}

export function waitSettled(peer, timeoutMs) {
  return peer.waitEvent((event) => event.type === "agent_settled", timeoutMs, "agent_settled");
}

// Readiness handshake: an extension command runs without an LLM call, so this
// proves guard version, tool allowlist, and bound identity before any paid work.
export async function probeReadiness(peer, { expect, timeoutMs = 60000 }) {
  const id = nonce();
  const isProbeNotify = (item) => item?.type === "extension_ui_request" && item.method === "notify" && typeof item.message === "string" && item.message.includes(id);
  // Subscribe BEFORE sending. Pi RPC emits notify (extension_ui_request) first, then
  // the prompt response. Awaiting the response first drops the notify and hangs 60s.
  const pendingNotify = peer.waitEvent(isProbeNotify, timeoutMs, "readiness probe result");
  pendingNotify.catch(() => { /* abandoned probe waits are cancelled */ });
  let response;
  try { response = await peer.request({ type: "prompt", message: `/conductor-probe ${id}` }, timeoutMs); }
  catch (error) { pendingNotify.cancel?.(); throw new Error(`readiness probe request failed: ${error.message}${peer.stderrTail ? `\n${peer.stderrTail}` : ""}`); }
  if (response.success !== true) { pendingNotify.cancel?.(); throw new Error(`readiness probe was rejected: ${response.error || "unknown error"}`); }
  let event;
  try { event = await pendingNotify; }
  catch (error) { throw new Error(`${error.message}${peer.stderrTail ? `\n${peer.stderrTail}` : ""}`); }
  let report; try { report = JSON.parse(event.message); } catch (error) { throw new Error(`readiness probe returned non-JSON: ${error.message}`); }
  if (report.type !== "conductor_ready" || report.nonce !== id) throw new Error("readiness probe nonce mismatch");
  const errors = [];
  if (!report.ready) errors.push(`guard refused authorization: ${report.reason}`);
  if (expect?.agent && report.agent !== expect.agent) errors.push(`agent identity mismatch: ${report.agent}`);
  if (expect?.piRunId && report.piRunId !== expect.piRunId) errors.push(`run identity mismatch: ${report.piRunId}`);
  if (expect?.guardVersion && report.version !== expect.guardVersion) errors.push(`guard version mismatch: ${report.version}`);
  if (expect?.tools) { const missing = expect.tools.filter((tool) => !(report.tools || []).includes(tool)); const extra = (report.tools || []).filter((tool) => !expect.tools.includes(tool)); if (missing.length) errors.push(`missing tools: ${missing.join(", ")}`); if (extra.length) errors.push(`unexpected tools: ${extra.join(", ")}`); }
  return { ok: errors.length === 0, errors, report };
}

// Static capability check for doctor: never claims a live run is possible.
// When a project root is supplied the guard must be installed IN that project; a
// kit copy is not a substitute, because the guard reads that project's state.
export function inspectRuntime({ root, kitRoot, piBinary } = {}) {
  const errors = []; const warnings = [];
  const binary = resolvePiBinary(piBinary);
  if (!binary) errors.push("pi binary not resolvable (set CONDUCTOR_PI_BIN)");
  const projectGuard = root ? path.join(root, ".pi", "extensions", "conductor-guard.ts") : null;
  const kitGuard = kitRoot ? path.join(kitRoot, "extension", "conductor-guard.ts") : null;
  let guardPath = null;
  if (projectGuard && fs.existsSync(projectGuard)) guardPath = projectGuard;
  else if (!root && kitGuard && fs.existsSync(kitGuard)) guardPath = kitGuard;
  else if (projectGuard) errors.push("conductor-guard.ts is not installed in the project's .pi/extensions; run the installer");
  else if (kitGuard && fs.existsSync(kitGuard)) warnings.push("only the kit guard is present; no project root was supplied");
  else errors.push("conductor-guard.ts is absent from both the project and the kit");
  let guardVersion = null;
  if (guardPath) { const match = fs.readFileSync(guardPath, "utf8").match(/CONDUCTOR_GUARD_VERSION\s*=\s*"([^"]+)"/); guardVersion = match?.[1] ?? null; if (!guardVersion) errors.push("guard version could not be read"); }
  const nodeOk = Number(process.versions.node.split(".")[0]) >= 22;
  if (!nodeOk) errors.push(`node ${process.version} is below the required major version 22`);
  return { ok: errors.length === 0, probed: false, binary: binary ?? null, guardPath, guardVersion, errors, warnings, reason: errors.length ? errors.join("; ") : "static capability check only; a live probe runs per child launch" };
}

// Default backend. Tests inject a fake with the same createPeer contract.
export const processBackend = { name: "process", createPeer: (invocation) => createProcessPeer(invocation) };

export async function launchManagedChild({ backend = processBackend, invocation, expect, probeTimeoutMs = 60000 }) {
  const peer = backend.createPeer(invocation);
  try {
    const readiness = await probeReadiness(peer, { expect, timeoutMs: probeTimeoutMs });
    if (!readiness.ok) throw new Error(`managed child is not ready: ${readiness.errors.join("; ")}`);
    return { peer, readiness };
  } catch (error) { await peer.close().catch(() => null); throw error; }
}

export async function collectUsage(peer, timeoutMs = 15000) {
  try {
    const response = await peer.request({ type: "get_session_stats" }, timeoutMs);
    const tokens = response?.data?.tokens;
    if (!tokens || !Number.isSafeInteger(tokens.input) || !Number.isSafeInteger(tokens.output)) return { usage: null, usageKnown: false };
    return { usage: { inputTokens: tokens.input, outputTokens: tokens.output }, usageKnown: true };
  } catch { return { usage: null, usageKnown: false }; }
}
