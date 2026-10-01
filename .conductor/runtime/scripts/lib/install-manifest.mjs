import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import os from "node:os";
import { spawnSync } from "node:child_process";
import { validateMap } from "../../extension/conductor-ownership.mjs";
import { parseStateText, formatStateText, validateState, atomicWrite } from "./state.mjs";
import { inspectRuntime } from "./rpc.mjs";

export function sha256File(file) { return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex"); }
export function compareVersions(a, b) { const pa = String(a).split(".").map(Number), pb = String(b).split(".").map(Number); for (let i = 0; i < 3; i += 1) { if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) - (pb[i] || 0); } return 0; }

function walk(directory, prefix = "") {
  const result = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    const rel = path.posix.join(prefix, entry.name); const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) result.push(...walk(absolute, rel)); else if (entry.isFile()) result.push(rel);
  }
  return result;
}

export function managedFiles(source) {
  const entries = [];
  const addTree = (folder, destination) => { for (const rel of walk(path.join(source, folder))) entries.push({ source: path.posix.join(folder, rel), destination: path.posix.join(destination, rel) }); };
  addTree("agents", ".pi/agents"); addTree("extension", ".pi/extensions"); addTree("schemas", ".conductor/runtime/schemas"); addTree("scripts/lib", ".conductor/runtime/scripts/lib");
  entries.push({ source: "extension/conductor-ownership.mjs", destination: ".conductor/runtime/extension/conductor-ownership.mjs" });
  entries.push({ source: "scripts/conductor.mjs", destination: ".conductor/runtime/scripts/conductor.mjs" });
  entries.push({ source: "VERSION", destination: ".conductor/runtime/VERSION" });
  return entries.sort((a, b) => a.destination.localeCompare(b.destination));
}

// Deterministic frontmatter parser: this strict subset (top-level scalar keys,
// inline `a, b` lists, and `- item` block lists) is what Pi agents use. Anything
// else — unterminated inline lists, orphan list items, unknown structures — fails
// the load instead of being silently misread.
export function parseFrontmatter(file, text) {
  const match = text.match(/^---\n([\s\S]*?)\n---\n/); if (!match) throw new Error(`${file}: missing frontmatter`);
  const fm = {}; let currentKey = null;
  for (const raw of match[1].split("\n")) {
    if (!raw.trim() || /^\s*#/.test(raw)) continue;
    const kv = raw.match(/^([A-Za-z][A-Za-z0-9_-]*):(?:[ \t]+(.*))?$/) ;
    if (kv && !/^[ \t]/.test(raw)) {
      const [, key, value = ""] = kv; if (Object.hasOwn(fm, key)) throw new Error(`${file}: duplicate frontmatter key '${key}'`);
      currentKey = key;
      if (value === "") { fm[key] = []; }
      else if (value.startsWith("[")) { if (!/^\[[^\[\]]*\]$/.test(value.trim())) throw new Error(`${file}: unterminated inline list for '${key}'`); fm[key] = value.trim().slice(1, -1).split(",").map((x) => x.trim()).filter(Boolean); }
      else fm[key] = value;
    } else if (/^[ \t]+-[ \t]+/.test(raw) || /^[ \t]+-[ \t]*$/.test(raw)) {
      const item = raw.trim().slice(1).trim(); if (!currentKey || !Array.isArray(fm[currentKey]) || !item) throw new Error(`${file}: list item without a list key or empty item`);
      fm[currentKey].push(item);
    } else if (/^[ \t]/.test(raw)) {
      if (!currentKey || Array.isArray(fm[currentKey])) throw new Error(`${file}: unexpected indented line under '${currentKey || "nothing"}'`);
      fm[currentKey] = `${fm[currentKey]} ${raw.trim()}`.trim();
    } else throw new Error(`${file}: malformed frontmatter line: ${raw}`);
  }
  return fm;
}

const KNOWN_TOOLS = new Set(["read", "grep", "find", "ls", "write", "edit", "bash", "conductor_evidence", "conductor_command", "conductor_report", "conductor_guard_status"]);

export function validateAgent(file, expectedName) {
  const text = fs.readFileSync(file, "utf8"); const fm = parseFrontmatter(file, text);
  if (fm.name !== expectedName) throw new Error(`${file}: frontmatter name '${fm.name}' does not match filename '${expectedName}'`);
  if (typeof fm.description !== "string" || !fm.description) throw new Error(`${file}: description is required`);
  const tools = Array.isArray(fm.tools) ? fm.tools : String(fm.tools || "").split(",").map((x) => x.trim()).filter(Boolean);
  if (!tools.length) throw new Error(`${file}: tools are required`);
  for (const tool of tools) if (!KNOWN_TOOLS.has(tool)) throw new Error(`${file}: unknown tool '${tool}'`);
  if (fm.acceptanceRole !== undefined && !["writer", "read-only"].includes(fm.acceptanceRole)) throw new Error(`${file}: acceptanceRole must be writer or read-only`);
  if (fm.toolTimeoutMs !== undefined && (!/^\d+$/.test(String(fm.toolTimeoutMs)) || Number(fm.toolTimeoutMs) <= 0)) throw new Error(`${file}: toolTimeoutMs must be a positive integer`);
  if (fm.completionGuard !== undefined && !["true", "false"].includes(String(fm.completionGuard))) throw new Error(`${file}: completionGuard must be boolean`);
  const refs = Array.isArray(fm.subagentOnlyExtensions) ? fm.subagentOnlyExtensions : fm.subagentOnlyExtensions ? [fm.subagentOnlyExtensions] : [];
  for (const ref of refs) {
    if (!ref.startsWith("../extensions/")) throw new Error(`${file}: subagentOnlyExtensions must reference ../extensions/ paths`);
    const direct = path.resolve(path.dirname(file), ref);
    const sourceLayout = direct.replace(`${path.sep}extensions${path.sep}`, `${path.sep}extension${path.sep}`);
    if (!fs.existsSync(direct) && !fs.existsSync(sourceLayout)) throw new Error(`${file}: missing subagentOnlyExtensions target ${ref}`);
  }
}

function findPiSubagentsRoot() {
  const candidates = [process.env.PI_SUBAGENTS_ROOT, path.join(os.homedir(), ".pi", "agent", "npm", "node_modules", "pi-subagents")].filter(Boolean);
  for (const directory of process.env.PATH?.split(path.delimiter) || []) {
    const executable = path.join(directory, "pi-subagents");
    if (fs.existsSync(executable)) { try { candidates.push(path.dirname(fs.realpathSync(executable))); } catch {} }
  }
  return candidates.find((candidate) => fs.existsSync(path.join(candidate, "src", "agents", "agents.ts"))) || null;
}

export function validateAgentsWithPiSubagents(source) {
  const packageRoot = findPiSubagentsRoot();
  if (!packageRoot) return { available: false, ok: true, reason: "pi-subagents runtime unavailable" };
  const jiti = [path.join(packageRoot, "node_modules", "jiti", "lib", "jiti.mjs"), path.join(packageRoot, "..", "jiti", "lib", "jiti.mjs")].find(fs.existsSync);
  if (!jiti) return { available: false, ok: true, reason: "pi-subagents runtime loader unavailable" };
  const sandbox = fs.mkdtempSync(path.join(os.tmpdir(), "conductor-agent-load-"));
  try {
    const agentDir = path.join(sandbox, ".pi", "agents"), extensionDir = path.join(sandbox, ".pi", "extensions");
    fs.mkdirSync(agentDir, { recursive: true }); fs.mkdirSync(extensionDir, { recursive: true });
    fs.cpSync(path.join(source, "agents"), agentDir, { recursive: true }); fs.cpSync(path.join(source, "extension"), extensionDir, { recursive: true });
    const expected = fs.readdirSync(agentDir).filter((name) => name.endsWith(".md")).map((name) => path.basename(name, ".md")).sort();
    const script = `import {pathToFileURL} from "node:url"; const {createJiti}=await import(pathToFileURL(process.env.JITI)); const jiti=createJiti(import.meta.url,{interopDefault:true}); const mod=await jiti.import(process.env.AGENTS_MODULE); const result=mod.discoverAgents(process.env.PROJECT_ROOT,"project"); const local=result.agents.filter((agent)=>agent.filePath?.startsWith(process.env.PROJECT_ROOT)); const diagnostics=result.agentDiagnostics.filter((item)=>JSON.stringify(item).includes(process.env.PROJECT_ROOT)); const actual=local.map((agent)=>agent.name).sort(); const expected=JSON.parse(process.env.EXPECTED); if(JSON.stringify(actual)!==JSON.stringify(expected)||diagnostics.length){console.error(JSON.stringify({expected,actual,diagnostics},null,2));process.exit(1);} console.log(JSON.stringify({loaded:actual.length,names:actual}));`;
    const result = spawnSync(process.execPath, ["--input-type=module", "-e", script], { encoding: "utf8", timeout: 30000, env: { ...process.env, JITI: jiti, AGENTS_MODULE: path.join(packageRoot, "src", "agents", "agents.ts"), PROJECT_ROOT: sandbox, EXPECTED: JSON.stringify(expected) } });
    if (result.status !== 0) return { available: true, ok: false, error: (result.stderr || result.stdout || `runtime exited ${result.status}`).trim() };
    return { available: true, ok: true, ...JSON.parse(result.stdout) };
  } finally { fs.rmSync(sandbox, { recursive: true, force: true }); }
}

export function validateKit(source) {
  const errors = []; const version = fs.readFileSync(path.join(source, "VERSION"), "utf8").trim();
  if (!/^\d+\.\d+\.\d+$/.test(version)) errors.push("VERSION must be semver");
  const skill = fs.readFileSync(path.join(source, "SKILL.md"), "utf8");
  if (!skill.includes(`conductor-version: "${version}"`)) errors.push("SKILL.md conductor-version does not match VERSION");
  for (const rel of walk(path.join(source, "agents"))) try { validateAgent(path.join(source, "agents", rel), path.basename(rel, ".md")); } catch (error) { errors.push(error.message); }
  for (const rel of ["extension/conductor-ownership.mjs", "scripts/conductor.mjs", ...walk(path.join(source, "scripts/lib")).map((x) => `scripts/lib/${x}`)]) {
    const check = spawnSync(process.execPath, ["--check", path.join(source, rel)], { encoding: "utf8" }); if (check.status !== 0) errors.push(`${rel}: syntax check failed: ${check.stderr.trim()}`);
  }
  const guardCheck = spawnSync(process.execPath, ["--experimental-strip-types", "--check", path.join(source, "extension/conductor-guard.ts")], { encoding: "utf8" });
  if (guardCheck.status !== 0) errors.push(`extension/conductor-guard.ts: syntax check failed: ${guardCheck.stderr.trim()}`);
  try { const parsed = parseStateText(fs.readFileSync(path.join(source, "templates/conductor-state.md"), "utf8")); errors.push(...validateState(parsed.state).map((x) => `state template: ${x}`)); } catch (error) { errors.push(`state template: ${error.message}`); }
  try { const map = JSON.parse(fs.readFileSync(path.join(source, "templates/section-map.json"), "utf8")); const result = validateMap(map); errors.push(...result.errors.map((x) => `map template: ${x}`)); } catch (error) { errors.push(`map template: ${error.message}`); }
  for (const rel of walk(path.join(source, "schemas"))) try { const schema = JSON.parse(fs.readFileSync(path.join(source, "schemas", rel), "utf8")); if (!schema.$schema || !schema.$id) errors.push(`${rel}: missing $schema/$id`); } catch (error) { errors.push(`${rel}: ${error.message}`); }
  const agentRuntime = validateAgentsWithPiSubagents(source); if (!agentRuntime.ok) errors.push(`pi-subagents agent load failed: ${agentRuntime.error}`);
  // Kit validity and execution readiness are separate claims. A kit can be
  // perfectly installable on a machine that cannot run a managed child, and
  // doctor must never report that as ready. Two execution paths exist: the legacy
  // pi-subagents async controls, or the RPC engine backend.
  const execution = inspectRuntime({ kitRoot: source });
  const executionPaths = { legacySubagents: agentRuntime.available === true, rpcEngine: execution.ok };
  const executionReady = executionPaths.legacySubagents || executionPaths.rpcEngine;
  return { ok: errors.length === 0, errors, version, agentRuntime, execution, executionPaths, executionReady };
}

function loadManifest(file) { if (!fs.existsSync(file)) return null; const value = JSON.parse(fs.readFileSync(file, "utf8")); if (value.schemaVersion !== 1 || !Array.isArray(value.files)) throw new Error("invalid prior install manifest"); return value; }
function timestamp() { return new Date().toISOString().replace(/[:.]/g, "-"); }

export function inspectDrift(source, root) {
  const manifestFile = path.join(root, ".conductor", "install.json"); const prior = loadManifest(manifestFile); const desired = managedFiles(source); const byDest = new Map((prior?.files || []).map((x) => [x.path, x])); const drift = [];
  for (const item of desired) { const target = path.join(root, item.destination); const old = byDest.get(item.destination); if (fs.existsSync(target) && (!old || sha256File(target) !== old.sha256)) drift.push({ path: item.destination, kind: old ? "modified" : "unmanaged-collision" }); }
  for (const old of prior?.files || []) if (!desired.some((x) => x.destination === old.path) && fs.existsSync(path.join(root, old.path)) && sha256File(path.join(root, old.path)) !== old.sha256) drift.push({ path: old.path, kind: "modified-removed" });
  return { prior, drift, desired };
}

export function installProject({ source, root, mode = "install", force = false }) {
  const validation = validateKit(source); if (!validation.ok) throw new Error(`source validation failed:\n${validation.errors.join("\n")}`);
  fs.mkdirSync(path.join(root, ".conductor"), { recursive: true });
  const inspected = inspectDrift(source, root); const priorVersion = inspected.prior?.installedVersion;
  if (mode === "update" && !inspected.prior) throw new Error("cannot update: no prior install manifest; run install --force to adopt legacy files");
  if (priorVersion && compareVersions(validation.version, priorVersion) < 0) throw new Error(`downgrade refused: ${priorVersion} -> ${validation.version}`);
  if (inspected.drift.length && !force) throw new Error(`managed-file drift detected:\n${inspected.drift.map((x) => `${x.kind}: ${x.path}`).join("\n")}\nRe-run with --force to back up drift.`);
  let backupRoot = inspected.drift.length ? path.join(root, ".conductor", "backups", timestamp()) : null;
  if (backupRoot) for (const item of inspected.drift) { const from = path.join(root, item.path); if (fs.existsSync(from)) { const to = path.join(backupRoot, item.path); fs.mkdirSync(path.dirname(to), { recursive: true }); fs.copyFileSync(from, to); } }
  const rollback = path.join(root, ".conductor", `.rollback-${process.pid}-${Date.now()}`); fs.mkdirSync(rollback, { recursive: true }); const touched = [];
  try {
    for (const item of inspected.desired) {
      const from = path.join(source, item.source), to = path.join(root, item.destination); fs.mkdirSync(path.dirname(to), { recursive: true });
      if (fs.existsSync(to)) { const save = path.join(rollback, item.destination); fs.mkdirSync(path.dirname(save), { recursive: true }); fs.copyFileSync(to, save); }
      touched.push(item.destination); atomicWrite(to, fs.readFileSync(from)); if (sha256File(to) !== sha256File(from)) throw new Error(`post-copy hash mismatch: ${item.destination}`);
    }
    for (const old of inspected.prior?.files || []) if (!inspected.desired.some((x) => x.destination === old.path)) { const target = path.join(root, old.path); if (fs.existsSync(target) && sha256File(target) === old.sha256) { const save = path.join(rollback, old.path); fs.mkdirSync(path.dirname(save), { recursive: true }); fs.copyFileSync(target, save); fs.unlinkSync(target); touched.push(old.path); } }
    const state = path.join(root, ".conductor-state.md"), stateRel = ".conductor-state.md"; const stateExisted = fs.existsSync(state); if (stateExisted) { const save = path.join(rollback, stateRel); fs.mkdirSync(path.dirname(save), { recursive: true }); fs.copyFileSync(state, save); } touched.push(stateRel);
    if (!stateExisted) atomicWrite(state, fs.readFileSync(path.join(source, "templates/conductor-state.md")));
    else {
      try { const parsed = parseStateText(fs.readFileSync(state, "utf8")); const stateErrors = validateState(parsed.state); if (stateErrors.length) throw new Error(stateErrors.join("; ")); }
      catch (error) {
        if (!force) throw new Error(`legacy/unversioned state requires explicit --force migration: ${error.message}`);
        backupRoot ||= path.join(root, ".conductor", "backups", timestamp()); const stateBackup = path.join(backupRoot, ".conductor-state.md"); fs.mkdirSync(path.dirname(stateBackup), { recursive: true }); fs.copyFileSync(state, stateBackup);
        const migratedText = fs.readFileSync(path.join(source, "templates/conductor-state.md"), "utf8"); const migrated = parseStateText(migratedText); migrated.state.report = { migration: { fromSchemaVersion: 0, toSchemaVersion: 2, steps: ["legacy-to-v1", "v1-to-v2"], backup: path.relative(root, stateBackup).replaceAll(path.sep, "/") } }; atomicWrite(state, formatStateText(migrated, migrated.state)); const verifyMigrated = parseStateText(fs.readFileSync(state, "utf8")); const migrationErrors = validateState(verifyMigrated.state); if (migrationErrors.length) throw new Error(`state migration postcondition failed: ${migrationErrors.join("; ")}`);
      }
    }
    const map = path.join(root, ".conductor", "section-map.json"); if (!fs.existsSync(map)) { touched.push(".conductor/section-map.json"); atomicWrite(map, fs.readFileSync(path.join(source, "templates/section-map.json"))); }
    const manifest = { schemaVersion: 1, installedVersion: validation.version, installedAt: new Date().toISOString(), source: "conductor", files: inspected.desired.map((item) => ({ path: item.destination, sha256: sha256File(path.join(root, item.destination)) })) };
    const manifestPath = path.join(root, ".conductor", "install.json"), manifestRel = ".conductor/install.json"; if (fs.existsSync(manifestPath)) { const save = path.join(rollback, manifestRel); fs.mkdirSync(path.dirname(save), { recursive: true }); fs.copyFileSync(manifestPath, save); } touched.push(manifestRel); atomicWrite(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
    const verify = inspectDrift(source, root); if (verify.drift.length) throw new Error(`post-install drift: ${verify.drift.map((x) => x.path).join(", ")}`);
    fs.rmSync(rollback, { recursive: true, force: true }); return { version: validation.version, backupRoot, files: manifest.files.length };
  } catch (error) {
    for (const rel of touched.reverse()) { const target = path.join(root, rel), saved = path.join(rollback, rel); if (fs.existsSync(saved)) { fs.mkdirSync(path.dirname(target), { recursive: true }); fs.copyFileSync(saved, target); } else fs.rmSync(target, { force: true }); }
    fs.rmSync(rollback, { recursive: true, force: true }); throw error;
  }
}
