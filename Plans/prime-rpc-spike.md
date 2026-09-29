# Spike: prime-agent as oozie worker (2026-08-09)

Question: can oozie's current `pi.Manager` drive `prime-agent --mode rpc`?

## Verdict

**Mostly same event dialect, not a drop-in.** Safe as a second backend after a thin adapter; unsafe as `PI_BIN=prime-agent` today.

## Compatible (same shapes oozie already handles)

| Event | pi | prime | Notes |
|---|---|---|---|
| `response` (prompt) | yes | yes | prime omits `data` on success sometimes |
| `agent_start` | yes | yes | |
| `message_start` / `message_end` | yes | yes | `message.role` + content blocks |
| `message_update` | yes | yes | **prime includes `message` (good for partials); pi often only `assistantMessageEvent`** |
| `tool_execution_start` / `_end` | yes | yes | same `toolCallId`, `toolName`, `args`, `result`, `isError` |
| `get_session_stats` | yes | yes | tokens/cost/contextUsage shape matches oozie's `handleStats` |
| `turn_start` / `turn_end` / `agent_end` | yes | yes | oozie ignores these today |

## Blockers for naive swap

1. **No `agent_settled` from prime** (observed). Oozie settles requests, improve-loop, and wish-fairy **only** on `agent_settled`. With prime, runs would complete in the agent but stay `streaming` in the DB forever (or until process exit path). **Adapter must treat `agent_end` as settle** (and still ignore duplicates).
2. **Auth/providers**: prime default is `xai-auth` / extension-backed models. `--no-extensions` → "No API key". Oozie must **not** strip prime's extensions; catalog must read `~/.prime/agent/settings.json` (`enabledModels`, `defaultProvider`).
3. **Tool culture**: prime preferred `ipython` over `read` for a simple file read. Approval extension gates `bash`/`write`/`edit` only — **ipython is unrestricted** unless the gate is expanded. Fairy/untrusted semantics need a Prime-aware policy.
4. **Binary + config home**: `prime-agent` + `~/.prime/agent`, not `pi` + `~/.pi/agent`.
5. **Extra events**: `tool_execution_update` (safe to ignore).

## Extension / permission gate

`internal/agent/pi/approval.ts` is pi-extension shaped and likely loads on prime (same family), but must be re-tested. Even if confirm works, **add `ipython` (and any prime-only mutators) to the gated set** for untrusted projects.

## Recommended integration path

1. Keep hygiene sweep for stranded `streaming` rows (done).
2. Extract `Agent` interface from `*pi.Manager`.
3. `prime` backend: same RPC IO, map `agent_end` → `RequestSettled`, load `.prime` catalog, keep extensions, expand approval gate.
4. Feature-flag `OOZIE_AGENT=prime|pi`; prove fairy + one full publish before default flip.

## Commands used in this spike

```bash
prime-agent --mode rpc --no-session --provider xai-auth --model grok-4.5
# stdin: {"type":"prompt","message":"..."}
# later: {"type":"get_session_stats"}
```
