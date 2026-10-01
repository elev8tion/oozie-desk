import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { spawnSync } from "node:child_process";
import { validateMap, resolveOwnership, compileGlob } from "../../extension/conductor-ownership.mjs";
import { atomicWrite, digest } from "./state.mjs";
import { validateAgent } from "./install-manifest.mjs";
import { expectedReportPath, readJsonl, validateReportEvent } from "./report.mjs";
import { safeTarget, secretPath } from "./context-paths.mjs";

const generated = (p) => /^(?:\.git|\.conductor|\.vnodes|node_modules|dist|build)(?:\/|$)/.test(p) || p === ".conductor-state.md";
function inventory(root) {
  if (fs.realpathSync(root) === fs.realpathSync(os.homedir())) throw new Error("refusing HOME inventory");
  // Do not inherit an enclosing repository's inventory.
  const top = spawnSync("git", ["rev-parse", "--show-toplevel"], { cwd: root, encoding: "utf8" });
  if (top.status === 0 && fs.realpathSync(top.stdout.trim()) === fs.realpathSync(root)) {
    const result = spawnSync("git", ["ls-files", "--cached", "--others", "--exclude-standard", "-z"], { cwd: root, encoding: "utf8", maxBuffer: 32 * 1024 * 1024 });
    if (result.status !== 0) throw new Error("git inventory failed");
    return [...new Set(result.stdout.split("\0").filter((p) => p && !generated(p)))].sort();
  }
  const files = [];
  const visit = (dir, prefix = "") => { for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const rel = prefix + entry.name; if (generated(rel)) continue;
    if (entry.isDirectory()) visit(path.join(dir, entry.name), `${rel}/`); else files.push(rel);
  } };
  visit(root); return files.sort();
}

export function checkMapReuse({ root, mapPath = ".conductor/section-map.json", future = [], refresh = false }) {
  const validatedAt = new Date().toISOString(); let sha256 = null;
  try {
    const mapFile = safeTarget(root, mapPath), text = fs.readFileSync(mapFile, "utf8"); sha256 = digest(text);
    const map = JSON.parse(text); const structural = validateMap(map);
    if (!structural.ok) return { ...structural, reusable: false, reason: "invalid-map", sha256, validatedAt };
    const files = inventory(root); const ignored = (map.ignored || []).map(compileGlob);
    const agents = []; const agentHashes = [];
    for (const agent of new Set(map.sections.map((s) => s.agent))) {
      const relative = `.pi/agents/${agent}.md`, file = safeTarget(root, relative);
      validateAgent(file, agent); agents.push(agent); agentHashes.push([relative, digest(fs.readFileSync(file))]);
    }
    const ordered = files.filter((p) => p !== mapPath).map((p) => {
      const file = safeTarget(root, p);
      let stat; try { stat = fs.lstatSync(file); } catch (error) { if (error.code !== "ENOENT") throw error; }
      // Contents, not HEAD or mtimes: includes dirty configuration of any framework.
      // Secret contents are never read; their presence still participates.
      return [p, !stat ? "deleted" : stat.isFile() ? (secretPath(p) ? "secret-present" : digest(fs.readFileSync(file))) : "non-file"];
    });
    for (const item of future) safeTarget(root, typeof item === "string" ? item : item.path);
    const candidates = files.filter((p) => p !== mapPath && !ignored.some((re) => re.test(p)));
    const result = validateMap(map, { candidates, future, availableAgents: agents });
    for (const item of future) {
      const p = typeof item === "string" ? item : item.path;
      if (resolveOwnership(map, p).kind !== "owned") result.errors.push(`future path '${p}' is not owned`);
    }
    result.ok = result.errors.length === 0;
    if (!result.ok) return { ...result, reusable: false, reason: "invalid-map", sha256, validatedAt };
    const fingerprint = digest(JSON.stringify({ sha256, inventory: ordered, agents: agentHashes.sort(), future }));
    const cacheFile = safeTarget(root, ".conductor/map-cache.json"); let cache;
    try { cache = JSON.parse(fs.readFileSync(cacheFile, "utf8")); } catch { /* absent/corrupt cache is only a miss */ }
    const trusted = cache?.schemaVersion === 1 && cache.valid === true && cache.fingerprint === fingerprint && cache.sha256 === sha256 && Number.isFinite(Date.parse(cache.validatedAt));
    const starter = !map.framework || /unknown|starter/i.test(String(map.framework)) || (map.notes || []).some((note) => /starter map.*(?:replaced|cartographer)/i.test(note));
    const reusable = !refresh && !starter && trusted;
    atomicWrite(cacheFile, `${JSON.stringify({ schemaVersion: 1, valid: !starter, sha256, fingerprint, validatedAt })}\n`);
    return { ...result, reusable, reason: starter ? "untrusted-framework-or-starter" : refresh ? "refresh-requested" : reusable ? "validated-cache-hit" : "validated-cache-miss", sha256, fingerprint, validatedAt, candidates: candidates.length };
  } catch (error) { return { ok: false, reusable: false, reason: "validation-failed", errors: [error.message], sha256, validatedAt }; }
}

export function buildContextPacket({ root, state, sectionId, piRunId, maxBytes = 48000 }) {
  if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0) throw new Error("maxBytes must be a positive integer");
  const section = state.sections?.[sectionId]; if (!section) throw new Error(`unknown section '${sectionId}'`);
  expectedReportPath(root, state.run.id, section.agent, piRunId);
  const workOrder = structuredClone(section.workOrder);
  const contracts = (workOrder.contracts || []).map((reference) => {
    const id = typeof reference === "string" ? reference : reference.id;
    const matches = (state.contracts || []).filter((contract) => contract.id === id);
    if (matches.length !== 1) throw new Error(`missing or duplicate referenced contract '${id}'`);
    return structuredClone(matches[0]);
  });
  const agentFile = safeTarget(root, `.pi/agents/${section.agent}.md`);
  const instructions = { agent: fs.readFileSync(agentFile, "utf8"), project: [] };
  // Only root instruction files; never discover source via directory sweeps.
  for (const relative of ["AGENTS.md", "CLAUDE.md"]) {
    const file = safeTarget(root, relative); if (fs.existsSync(file)) instructions.project.push({ path: relative, content: fs.readFileSync(file, "utf8") });
  }
  const checkpoints = [];
  for (const attempt of section.attempts || []) {
    if (!attempt.piRunId || attempt.piRunId === piRunId) continue;
    const expected = expectedReportPath(root, state.run.id, section.agent, attempt.piRunId);
    const relative = path.relative(path.resolve(root), expected).split(path.sep).join("/");
    const file = safeTarget(root, relative);
    if (attempt.reportPath && path.resolve(root, attempt.reportPath) !== path.resolve(expected)) throw new Error("checkpoint report identity mismatch");
    const events = readJsonl(file); let last;
    for (const [index, event] of events.entries()) {
      const errors = validateReportEvent(event);
      if (errors.length || event.seq !== index + 1 || (last && (last.kind === "final" || Date.parse(event.at) < Date.parse(last.at)))) throw new Error("invalid checkpoint history");
      last = event;
    }
    checkpoints.push({ piRunId: attempt.piRunId, outcome: attempt.outcome ?? null, latest: events.findLast((e) => e.kind === "checkpoint") ?? null, final: events.findLast((e) => e.kind === "final") ?? null, deviations: events.filter((e) => e.kind === "deviation" && e.resolved !== true) });
  }
  const source = [];
  for (const relative of new Set([...(workOrder.declaredFiles || []), ...(workOrder.contextFiles || [])])) {
    const file = safeTarget(root, relative);
    if (secretPath(relative)) { source.push({ path: relative, excluded: "secret-default" }); continue; }
    if (!fs.existsSync(file)) { source.push({ path: relative, missing: true }); continue; }
    if (!fs.statSync(file).isFile()) throw new Error(`context target is not a file: ${relative}`);
    source.push({ path: relative, content: "", truncated: true, bytes: fs.statSync(file).size });
  }
  const packet = { schemaVersion: 1, runId: state.run.id, sectionId, piRunId, workOrder, contracts, instructions, source, checkpoints };
  const size = () => Buffer.byteLength(JSON.stringify(packet));
  if (size() > maxBytes) throw new Error("mandatory instructions/contracts/checkpoints exceed maxBytes");
  // Fair, bounded excerpts; JSON escaping and multibyte text count toward the cap.
  const excerpts = source.filter((item) => Object.hasOwn(item, "content"));
  for (const [index, item] of excerpts.entries()) {
    const budget = Math.floor((maxBytes - size()) / (excerpts.length - index));
    const fd = fs.openSync(safeTarget(root, item.path), "r"); const buffer = Buffer.alloc(Math.min(item.bytes, budget));
    let count; try { count = fs.readSync(fd, buffer); } finally { fs.closeSync(fd); }
    const text = buffer.subarray(0, count).toString("utf8"); let low = 0, high = text.length;
    while (low < high) { const mid = Math.ceil((low + high) / 2); item.content = text.slice(0, mid); if (size() <= maxBytes) low = mid; else high = mid - 1; }
    item.content = text.slice(0, low); item.truncated = count < item.bytes || low < text.length;
    if (size() > maxBytes) { item.truncated = true; }
  }
  if (size() > maxBytes) throw new Error("packet exceeds maxBytes");
  return packet;
}
