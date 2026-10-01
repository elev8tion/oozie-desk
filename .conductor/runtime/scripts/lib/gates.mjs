// Shared promotion gates.
//
// Both the coordinator CLI and the execution engine must enforce identical
// evidence requirements, so the gates live here instead of in one caller. An
// engine-driven run therefore cannot reach build-done with weaker proof than a
// coordinator-driven run.
import fs from "node:fs";
import path from "node:path";
import { deriveActiveWaves } from "../../extension/conductor-ownership.mjs";
import { taskWaves } from "./state.mjs";
import { expectedReportPath, readJsonl, validateReportEvent } from "./report.mjs";

function sameJson(a, b) { return JSON.stringify(a) === JSON.stringify(b); }

// Ownership and task ordering are different questions. The map stays the sole
// authority on who may edit a file; in ready scheduling mode the per-task DAG
// decides what must finish first, so an architecturally downstream section is not
// forced to wait when this task does not actually depend on it.
export function assertPlanAgreement(state, map) {
  const activeIds = Object.keys(state.sections).sort();
  const byId = new Map(map.sections.filter((section) => section.enabled !== false).map((section) => [section.id, section]));
  const ready = state.scheduling?.mode === "ready";
  for (const id of activeIds) {
    const mapped = byId.get(id); if (!mapped) throw new Error(`state section '${id}' is not enabled in the promoted map`);
    const section = state.sections[id]; if (section.agent !== mapped.agent) throw new Error(`state section '${id}' agent does not match promoted map`);
    if (ready) {
      for (const dep of section.dependsOn) if (!activeIds.includes(dep)) throw new Error(`state section '${id}' has task dependency '${dep}' outside the active plan`);
      continue;
    }
    const expectedDeps = (mapped.dependsOn || []).filter((dep) => activeIds.includes(dep)).sort();
    const actualDeps = [...section.dependsOn].sort();
    if (!sameJson(actualDeps, expectedDeps)) throw new Error(`state section '${id}' dependencies do not match promoted map DAG`);
  }
  if (ready) {
    const expected = taskWaves(state.sections);
    if (!sameJson(state.waves, expected)) throw new Error(`state waves do not match the task DAG projection: expected ${JSON.stringify(expected)}`);
    return;
  }
  const expectedWaves = deriveActiveWaves(map, activeIds);
  if (!sameJson(state.waves, expectedWaves)) throw new Error(`state waves do not match promoted map DAG: expected ${JSON.stringify(expectedWaves)}`);
}

// A writer is done only on checked evidence: an identity-bound report that ends in
// a completed final with nothing remaining, no unresolved deviation or blocker,
// and every required declared command passed. Prose is never evidence.
export function assertWriterEvidence(state, projectRoot) {
  for (const [sectionId, section] of Object.entries(state.sections)) {
    if (section.status === "skipped") continue;
    if (section.status !== "done") throw new Error(`section '${sectionId}' is not done`);
    const attempt = section.attempts.at(-1);
    if (!attempt || !["checked", "verified"].includes(attempt.acceptance?.status)) throw new Error(`section '${sectionId}' lacks checked/verified writer acceptance`);
    const expected = expectedReportPath(projectRoot, state.run.id, section.agent, attempt.piRunId);
    const report = path.resolve(projectRoot, attempt.reportPath || "");
    if (report !== path.resolve(expected)) throw new Error(`section '${sectionId}' report path is not identity-bound to its latest attempt`);
    const events = readJsonl(report); if (!events.length) throw new Error(`section '${sectionId}' final report is missing`);
    for (const [index, event] of events.entries()) {
      const errors = validateReportEvent(event);
      if (errors.length) throw new Error(`section '${sectionId}' report event ${index + 1} is invalid: ${errors.join("; ")}`);
    }
    const final = events.at(-1);
    if (final.kind !== "final" || final.outcome !== "completed" || final.remaining.length) throw new Error(`section '${sectionId}' latest report does not end in completed final with no remaining work`);
    const resolved = new Set(section.resolvedEvents || []);
    for (const event of events) {
      const key = `${attempt.piRunId}:${event.seq}`;
      if (event.kind === "deviation" && event.resolved !== true && !resolved.has(key)) throw new Error(`section '${sectionId}' has unresolved deviation ${key}`);
      if (event.kind === "checkpoint" && ["blocked", "failed"].includes(event.status) && !resolved.has(key)) throw new Error(`section '${sectionId}' has unresolved blocker ${key}`);
    }
    for (const command of section.workOrder.commands.filter((item) => item.required !== false)) {
      if (!attempt.checks?.some((check) => check.id === command.id && check.status === "passed")) throw new Error(`section '${sectionId}' declared check '${command.id}' did not pass`);
    }
  }
}

export function assertBuildSettled(projectRoot) {
  if (fs.existsSync(path.join(projectRoot, ".conductor", "active.json"))) throw new Error("build-done requires settled active state");
}
