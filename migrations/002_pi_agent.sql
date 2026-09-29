ALTER TABLE agent_sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN pi_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_pending_questions ADD COLUMN rpc_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_permission_requests ADD COLUMN rpc_id TEXT NOT NULL DEFAULT '';
