import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";

const SECTION_ID = /^[a-z][a-z0-9-]{0,63}$/;
const MAGIC = /[*?[]/;

export function normalizeRelative(raw) {
  if (typeof raw !== "string" || !raw.trim()) throw new Error("path must be a non-empty string");
  let value = raw.trim().replace(/^@/, "").replaceAll("\\", "/");
  if (value.startsWith("/") || /^[A-Za-z]:\//.test(value)) throw new Error(`absolute path is unsafe: ${raw}`);
  value = path.posix.normalize(value).replace(/^\.\//, "");
  if (value === ".." || value.startsWith("../") || value.split("/").includes("..")) throw new Error(`path traversal is unsafe: ${raw}`);
  if (value === "." || value === "") throw new Error(`path does not name a file: ${raw}`);
  return value;
}

export function validateGlob(raw) {
  const glob = normalizeRelative(raw);
  if (glob.includes("\0") || glob.startsWith("!") || glob.includes("{") || glob.includes("}")) throw new Error(`unsupported glob: ${raw}`);
  if (glob === "none" || glob.endsWith("/none")) throw new Error(`use no rule for an empty section, not '${raw}'`);
  if (/\*\*\*/.test(glob)) throw new Error(`invalid glob: ${raw}`);
  return glob;
}

export function compileGlob(raw) {
  const glob = validateGlob(raw);
  let out = "^";
  for (let i = 0; i < glob.length; i += 1) {
    const c = glob[i];
    if (c === "*") {
      if (glob[i + 1] === "*") {
        i += 1;
        if (glob[i + 1] === "/") { i += 1; out += "(?:.*/)?"; }
        else out += ".*";
      } else out += "[^/]*";
    } else if (c === "?") out += "[^/]";
    else if (c === "[") {
      const end = glob.indexOf("]", i + 1);
      if (end < 0) throw new Error(`unclosed character class in glob: ${raw}`);
      const body = glob.slice(i + 1, end);
      if (!body || body.includes("/")) throw new Error(`invalid character class in glob: ${raw}`);
      out += `[${body.replace(/^!/, "^")}]`; i = end;
    } else out += c.replace(/[.+^${}()|\\]/g, "\\$&");
  }
  return new RegExp(`${out}$`);
}

export function globSpecificity(glob) {
  const g = validateGlob(glob);
  const literal = g.replace(/\*\*|\*|\?|\[[^\]]*\]/g, "");
  const segments = g.split("/");
  const literalSegments = segments.filter((part) => !MAGIC.test(part)).length;
  const magic = (g.match(/\*\*|\*|\?|\[/g) || []).length;
  return literalSegments * 10000 + literal.length * 10 - magic;
}

function literalPrefix(glob) {
  const index = glob.search(MAGIC);
  return (index < 0 ? glob : glob.slice(0, index)).replace(/\/$/, "");
}

function mayOverlap(a, b) {
  if (a === b) return true;
  const ap = literalPrefix(a); const bp = literalPrefix(b);
  return !ap || !bp || ap.startsWith(bp) || bp.startsWith(ap);
}

export function mapDigestInput(map) {
  return `${JSON.stringify(map)}\n`;
}

export function validateMap(map, options = {}) {
  const errors = []; const warnings = [];
  if (!map || typeof map !== "object" || Array.isArray(map)) return { ok: false, errors: ["map must be an object"], warnings, waves: [] };
  if (map.schemaVersion !== 2) errors.push("map.schemaVersion must be 2");
  if (typeof map.conductorVersion !== "string" || !map.conductorVersion) errors.push("map.conductorVersion is required");
  if (!Array.isArray(map.sections)) errors.push("map.sections must be an array");
  if (!Array.isArray(map.rules)) errors.push("map.rules must be an array");
  const ids = new Set(); const agents = new Set();
  for (const [index, section] of (Array.isArray(map.sections) ? map.sections : []).entries()) {
    if (!section || typeof section !== "object") { errors.push(`sections[${index}] must be an object`); continue; }
    if (!SECTION_ID.test(section.id || "")) errors.push(`invalid section id at sections[${index}]`);
    else if (ids.has(section.id)) errors.push(`duplicate section id '${section.id}'`); else ids.add(section.id);
    if (!SECTION_ID.test(section.agent || "")) errors.push(`invalid agent name for section '${section.id || index}'`);
    else if (section.enabled !== false && agents.has(section.agent)) errors.push(`enabled agent '${section.agent}' is bound to more than one section`);
    else if (section.enabled !== false) agents.add(section.agent);
    if (!Array.isArray(section.dependsOn) || section.dependsOn.some((x) => typeof x !== "string")) errors.push(`section '${section.id || index}' dependsOn must be a string array`);
    if (section.enabled !== undefined && typeof section.enabled !== "boolean") errors.push(`section '${section.id || index}' enabled must be boolean`);
  }
  for (const section of (map.sections || [])) for (const dep of (section.dependsOn || [])) if (!ids.has(dep)) errors.push(`section '${section.id}' has unknown dependency '${dep}'`);
  const ruleIds = new Set(); const compiled = [];
  for (const [index, rule] of (Array.isArray(map.rules) ? map.rules : []).entries()) {
    if (!rule || typeof rule !== "object") { errors.push(`rules[${index}] must be an object`); continue; }
    const id = rule.id || `rule-${index + 1}`;
    if (ruleIds.has(id)) errors.push(`duplicate rule id '${id}'`); else ruleIds.add(id);
    if (!ids.has(rule.owner)) errors.push(`rule '${id}' has unknown owner '${rule.owner}'`);
    try { compiled.push({ ...rule, id, index, glob: validateGlob(rule.glob), regex: compileGlob(rule.glob), specificity: globSpecificity(rule.glob) }); }
    catch (error) { errors.push(`rule '${id}': ${error.message}`); }
  }
  if (map.ignored !== undefined && !Array.isArray(map.ignored)) errors.push("map.ignored must be an array");
  for (const ignored of (Array.isArray(map.ignored) ? map.ignored : [])) try { validateGlob(ignored); } catch (error) { errors.push(`ignored rule: ${error.message}`); }
  for (let i = 0; i < compiled.length; i += 1) for (let j = i + 1; j < compiled.length; j += 1) {
    const first = compiled[i], later = compiled[j];
    if (first.owner !== later.owner && mayOverlap(first.glob, later.glob) && later.specificity > first.specificity) errors.push(`precedence: more-specific rule '${later.id}' must precede broader rule '${first.id}'`);
    if (first.owner === later.owner && first.glob === later.glob) warnings.push(`redundant rules '${first.id}' and '${later.id}'`);
  }
  const enabled = new Set((map.sections || []).filter((s) => s.enabled !== false).map((s) => s.id));
  const indegree = new Map([...enabled].map((id) => [id, 0])); const next = new Map([...enabled].map((id) => [id, []]));
  for (const section of (map.sections || []).filter((s) => enabled.has(s.id))) for (const dep of section.dependsOn || []) if (enabled.has(dep)) { indegree.set(section.id, indegree.get(section.id) + 1); next.get(dep).push(section.id); }
  const waves = []; let ready = [...enabled].filter((id) => indegree.get(id) === 0).sort(); let seen = 0;
  while (ready.length) { waves.push(ready); const after = []; for (const id of ready) { seen += 1; for (const child of next.get(id).sort()) { indegree.set(child, indegree.get(child) - 1); if (indegree.get(child) === 0) after.push(child); } } ready = [...new Set(after)].sort(); }
  if (seen !== enabled.size) errors.push("section dependency graph contains a cycle");
  if (options.availableAgents) for (const section of (map.sections || [])) if (!options.availableAgents.includes(section.agent)) errors.push(`section '${section.id}' references unavailable agent '${section.agent}'`);
  const candidateSet = new Set([...(options.candidates || []), ...((options.future || []).map((x) => typeof x === "string" ? x : x.path))]);
  for (const candidate of [...candidateSet].sort()) {
    let result;
    try { result = resolveOwnership(map, candidate); } catch (error) { errors.push(`candidate '${candidate}': ${error.message}`); continue; }
    if (result.kind === "ambiguous") errors.push(`candidate '${candidate}' is ambiguous: ${result.matches.map((x) => x.id).join(", ")}`);
    if (result.kind === "unmapped" && (options.candidates || []).includes(candidate)) errors.push(`candidate '${candidate}' is unmapped`);
  }
  for (const item of options.future || []) if (typeof item === "object") {
    const result = resolveOwnership(map, item.path);
    if (result.kind !== "owned" || result.owner !== item.owner) errors.push(`future path '${item.path}' resolves to '${result.kind === "owned" ? result.owner : result.kind}', expected '${item.owner}'`);
  }
  return { ok: errors.length === 0, errors, warnings, waves };
}

export function resolveOwnership(map, rawPath) {
  const rel = normalizeRelative(rawPath);
  const matches = [];
  for (const [index, rule] of (map.rules || []).entries()) {
    const regex = compileGlob(rule.glob);
    if (regex.test(rel)) matches.push({ id: rule.id || `rule-${index + 1}`, owner: rule.owner, glob: rule.glob, index, specificity: globSpecificity(rule.glob) });
  }
  if (!matches.length) return { kind: "unmapped", path: rel, matches: [] };
  const winner = matches[0];
  const reversed = matches.find((x) => x.owner !== winner.owner && x.specificity > winner.specificity);
  const tied = matches.find((x) => x.owner !== winner.owner && x.specificity === winner.specificity);
  if (reversed || tied) return { kind: "ambiguous", path: rel, matches, reason: reversed ? "a later rule is more specific" : "equal-specificity cross-owner rules" };
  return { kind: "owned", path: rel, owner: winner.owner, ruleId: winner.id, matches };
}

export function canonicalTarget(root, rawPath) {
  const rootReal = fs.realpathSync(root);
  const cleaned = String(rawPath || "").replace(/^@/, "");
  const absolute = path.isAbsolute(cleaned) ? path.resolve(cleaned) : path.resolve(rootReal, cleaned);
  let cursor = absolute; const suffix = [];
  while (!fs.existsSync(cursor)) { const parent = path.dirname(cursor); if (parent === cursor) break; suffix.unshift(path.basename(cursor)); cursor = parent; }
  const ancestor = fs.realpathSync(cursor);
  const target = path.resolve(ancestor, ...suffix);
  const relNative = path.relative(rootReal, target);
  if (!relNative || relNative.startsWith("..") || path.isAbsolute(relNative)) return { inside: false, absolute: target, relative: relNative.replaceAll(path.sep, "/") };
  return { inside: true, absolute: target, relative: relNative.replaceAll(path.sep, "/") };
}

export function deriveActiveWaves(map, sectionIds) {
  const result = validateMap(map);
  if (!result.ok) throw new Error(result.errors.join("; "));
  const requested = new Set(sectionIds);
  const known = new Map(map.sections.filter((section) => section.enabled !== false).map((section) => [section.id, section]));
  for (const id of requested) if (!known.has(id)) throw new Error(`active section '${id}' is not enabled in the map`);
  const indegree = new Map([...requested].map((id) => [id, 0])); const next = new Map([...requested].map((id) => [id, []]));
  for (const id of requested) for (const dep of known.get(id).dependsOn || []) if (requested.has(dep)) { indegree.set(id, indegree.get(id) + 1); next.get(dep).push(id); }
  const waves = []; let ready = [...requested].filter((id) => indegree.get(id) === 0).sort(); let seen = 0;
  while (ready.length) { waves.push({ id: `wave-${waves.length + 1}`, sections: ready }); const after = []; for (const id of ready) { seen += 1; for (const child of next.get(id).sort()) { indegree.set(child, indegree.get(child) - 1); if (indegree.get(child) === 0) after.push(child); } } ready = [...new Set(after)].sort(); }
  if (seen !== requested.size) throw new Error("active section dependency graph contains a cycle");
  return waves;
}

export function resolveActiveExecution(active, agent, piRunId) {
  if (!active || !Array.isArray(active.executions) || typeof agent !== "string" || !agent || typeof piRunId !== "string" || !piRunId) return null;
  return active.executions.find((item) => item.agent === agent && item.piRunId === piRunId) || null;
}

// Snapshot authorization ignores unrelated revisions, never fresh identity/revocation or content.
export function workOrderDigest(state, sectionId) {
  const section = state.sections[sectionId];
  return crypto.createHash("sha256").update(JSON.stringify({ runId: state.run.id, map: state.map, sectionId, agent: section.agent, dependsOn: section.dependsOn, workOrder: section.workOrder, contracts: state.contracts })).digest("hex");
}

export function mapperDigest(state) {
  return crypto.createHash("sha256").update(JSON.stringify({ runId: state.run.id, phase: "map", agent: "cartographer", mapPath: state.map?.path || ".conductor/section-map.json" })).digest("hex");
}

export function assertExecutionAuthorization(state, active, execution) {
  if (!execution || active?.schemaVersion !== 1 || active.runId !== state.run.id) throw new Error("active identity mismatch");
  if (active.phase === "map") {
    if (execution.agent !== "cartographer" || execution.section !== "cartographer" || !execution.piRunId) throw new Error("stale, revoked or inactive identity");
    if (active.authorization === "snapshot") {
      if (execution.authorizationDigest !== mapperDigest(state)) throw new Error("immutable work-order authorization mismatch");
      if (!/^[a-f0-9]{64}$/.test(execution.token || "")) throw new Error("execution token mismatch");
    }
    return execution;
  }
  if (!["build", "verify", "repair"].includes(active.phase)) throw new Error("active identity mismatch");
  const section = state.sections?.[execution.section], attempt = section?.attempts?.at(-1);
  if (!section || section.status !== "in-progress" || section.agent !== execution.agent || !execution.piRunId || attempt?.piRunId !== execution.piRunId || attempt.revokedAt) throw new Error("stale, revoked or inactive identity");
  if (JSON.stringify([...(execution.declaredFiles || [])].sort()) !== JSON.stringify([...section.workOrder.declaredFiles].sort())) throw new Error("declared files mismatch");
  if (active.authorization === "snapshot") {
    if (!attempt.authorizationDigest || execution.authorizationDigest !== attempt.authorizationDigest || execution.authorizationDigest !== workOrderDigest(state, execution.section)) throw new Error("immutable work-order authorization mismatch");
    if (execution.token !== attempt.token || !/^[a-f0-9]{64}$/.test(execution.token || "")) throw new Error("execution token mismatch");
    for (const dep of section.dependsOn) if (!["done", "skipped"].includes(state.sections[dep]?.status)) throw new Error("dependency no longer accepted");
  } else if (active.stateRevision !== state.revision || !state.waves.some(w => w.id === active.waveId)) throw new Error("active/state revision mismatch");
  return execution;
}

export function deriveWaves(map) {
  return deriveActiveWaves(map, map.sections.filter((section) => section.enabled !== false).map((section) => section.id));
}
