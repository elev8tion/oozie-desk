package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Actor is the single local operator. Queries use it instead of a hardcoded user.
type Actor struct {
	UserID int64
	OrgID  int64
}

type Repo struct {
	db    *sql.DB
	actor Actor
}

// parseSQLiteTime handles the formats SQLite emits for datetime values
// that reach us as text (expression results have no declared type).
func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02 15:04:05.999999999-07:00"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

func NewRepo(db *sql.DB) *Repo {
	r := &Repo{db: db, actor: Actor{UserID: 1, OrgID: 1}}
	r.loadActor(context.Background())
	return r
}

func (r *Repo) loadActor(ctx context.Context) {
	var userID, orgID int64
	if err := r.db.QueryRowContext(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&userID); err == nil && userID > 0 {
		r.actor.UserID = userID
	}
	if err := r.db.QueryRowContext(ctx, `SELECT id FROM organizations ORDER BY id LIMIT 1`).Scan(&orgID); err == nil && orgID > 0 {
		r.actor.OrgID = orgID
	}
}

func (r *Repo) Actor() Actor { return r.actor }

func (r *Repo) ListProjects(ctx context.Context, q, filter string) ([]Project, error) {
	where := []string{"1=1"}
	args := []any{}
	if q != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+q+"%")
	}
	if filter == "archived" {
		where = append(where, "archived = 1")
	} else if filter == "active" || filter == "" {
		where = append(where, "archived = 0")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, owner_user_id, organization_id, name, project_path_display, trusted, archived, status, created_at, updated_at FROM projects WHERE `+strings.Join(where, " AND ")+` ORDER BY updated_at DESC LIMIT 25`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var org sql.NullInt64
		if err := rows.Scan(&p.ID, &p.OwnerUserID, &org, &p.Name, &p.ProjectPathDisplay, &p.Trusted, &p.Archived, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if org.Valid {
			p.OrganizationID = &org.Int64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetProject(ctx context.Context, id int64) (Project, error) {
	var p Project
	var org sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT id, owner_user_id, organization_id, name, project_path_display, trusted, archived, status, created_at, updated_at FROM projects WHERE id=?`, id).Scan(&p.ID, &p.OwnerUserID, &org, &p.Name, &p.ProjectPathDisplay, &p.Trusted, &p.Archived, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if org.Valid {
		p.OrganizationID = &org.Int64
	}
	return p, err
}

func (r *Repo) CreateProject(ctx context.Context, name, path string, trusted bool) (Project, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO projects (owner_user_id, organization_id, name, project_path_display, trusted, archived, status) VALUES (?,?,?,?,?,?,?)`, r.actor.UserID, r.actor.OrgID, name, path, trusted, false, "ready")
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	_, _ = r.db.ExecContext(ctx, `INSERT INTO agent_sessions (project_id,title) VALUES (?,?)`, id, "Build session")
	return r.GetProject(ctx, id)
}

// DeleteProject removes the project row; sessions, requests, messages,
// prompts, drafts, and jobs cascade with it (store apps are handled by
// the service first, since their FK is SET NULL).
func (r *Repo) DeleteProject(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	return err
}

// StoreAppIDForProject returns the app published from this project, or 0.
func (r *Repo) StoreAppIDForProject(ctx context.Context, projectID int64) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM store_apps WHERE project_id=?`, projectID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (r *Repo) SetTrusted(ctx context.Context, id int64, trusted bool) error {
	_, err := r.db.ExecContext(ctx, `UPDATE projects SET trusted=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, trusted, id)
	return err
}

func (r *Repo) ArchiveProject(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE projects SET archived=1, status='archived', updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	return err
}

func (r *Repo) GetSession(ctx context.Context, projectID int64) (AgentSession, error) {
	var s AgentSession
	err := r.db.QueryRowContext(ctx, `SELECT id, project_id, title, model, pi_session_id, created_at, updated_at FROM agent_sessions WHERE project_id=? ORDER BY id DESC LIMIT 1`, projectID).Scan(&s.ID, &s.ProjectID, &s.Title, &s.Model, &s.PiSessionID, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		_, e := r.db.ExecContext(ctx, `INSERT INTO agent_sessions (project_id,title) VALUES (?,?)`, projectID, "Build session")
		if e != nil {
			return s, e
		}
		return r.GetSession(ctx, projectID)
	}
	return s, err
}

func (r *Repo) ListMessages(ctx context.Context, sessionID int64) ([]AgentMessage, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT m.id,m.request_id,m.role,m.status,m.content,m.metadata_json,COALESCE(json_extract(m.metadata_json,'$.body'),''),m.created_at FROM agent_messages m JOIN agent_requests ar ON ar.id=m.request_id WHERE ar.session_id=? ORDER BY m.created_at, m.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentMessage
	for rows.Next() {
		var m AgentMessage
		if err := rows.Scan(&m.ID, &m.RequestID, &m.Role, &m.Status, &m.Content, &m.MetadataJSON, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) ListRequests(ctx context.Context, sessionID int64) ([]AgentRequest, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,session_id,status,mode,created_at,updated_at,completed_at FROM agent_requests WHERE session_id=? ORDER BY id DESC LIMIT 10`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRequest
	for rows.Next() {
		var ar AgentRequest
		if err := rows.Scan(&ar.ID, &ar.SessionID, &ar.Status, &ar.Mode, &ar.CreatedAt, &ar.UpdatedAt, &ar.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, ar)
	}
	return out, rows.Err()
}

func (r *Repo) CreateAgentRequest(ctx context.Context, sessionID int64, mode, message string) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// A new request while one streams is a steer: the old request's events
	// now flow to the new one, so settle the old row.
	if _, err := tx.ExecContext(ctx, `UPDATE agent_requests SET status='completed', updated_at=CURRENT_TIMESTAMP, completed_at=CURRENT_TIMESTAMP WHERE session_id=? AND status IN ('streaming','waiting')`, sessionID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO agent_requests (session_id,status,mode) VALUES (?,'streaming',?)`, sessionID, mode)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_messages (request_id,role,status,content,metadata_json) VALUES (?,'user','completed',?,'{}')`, id, message); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (r *Repo) InsertMessage(ctx context.Context, requestID int64, role, status, content string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO agent_messages (request_id,role,status,content,metadata_json) VALUES (?,?,?,?,'{}')`, requestID, role, status, content)
	return err
}

// InsertToolStart records a tool call in flight; FinalizeTool (matched by
// callID) replaces it with the outcome and expandable body.
func (r *Repo) InsertToolStart(ctx context.Context, requestID int64, callID, content string) error {
	meta, _ := json.Marshal(map[string]string{"callId": callID})
	_, err := r.db.ExecContext(ctx, `INSERT INTO agent_messages (request_id,role,status,content,metadata_json) VALUES (?,'tool','loading',?,?)`, requestID, content, string(meta))
	return err
}

func (r *Repo) FinalizeTool(ctx context.Context, requestID int64, callID, content, body string) error {
	meta, _ := json.Marshal(map[string]string{"callId": callID, "body": body})
	res, err := r.db.ExecContext(ctx, `UPDATE agent_messages SET content=?, status='completed', metadata_json=? WHERE request_id=? AND role='tool' AND status='loading' AND json_extract(metadata_json,'$.callId')=?`, content, string(meta), requestID, callID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO agent_messages (request_id,role,status,content,metadata_json) VALUES (?,'tool','completed',?,?)`, requestID, content, string(meta))
	return err
}

// UpsertPartialAssistant keeps a single 'loading' assistant row per request
// updated with the streaming text-so-far.
func (r *Repo) UpsertPartialAssistant(ctx context.Context, requestID int64, content string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE agent_messages SET content=? WHERE request_id=? AND role='assistant' AND status='loading'`, content, requestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return r.InsertMessage(ctx, requestID, "assistant", "loading", content)
}

// FinalizeAssistant replaces the streaming 'loading' row with the final
// message text, or inserts a completed row if streaming never produced one.
func (r *Repo) FinalizeAssistant(ctx context.Context, requestID int64, content string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE agent_messages SET content=?, status='completed' WHERE request_id=? AND role='assistant' AND status='loading'`, content, requestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return r.InsertMessage(ctx, requestID, "assistant", "completed", content)
}

// FinalAssistantMessage returns the last completed assistant message of a
// request, or "" if the agent left none.
func (r *Repo) FinalAssistantMessage(ctx context.Context, requestID int64) (string, error) {
	var content string
	err := r.db.QueryRowContext(ctx, `SELECT content FROM agent_messages WHERE request_id = ? AND role = 'assistant' AND status = 'completed' ORDER BY created_at DESC, id DESC LIMIT 1`, requestID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return content, err
}

// FirstUserMessage is the prompt that started a request (for credit retries).
func (r *Repo) FirstUserMessage(ctx context.Context, requestID int64) (string, error) {
	var content string
	err := r.db.QueryRowContext(ctx, `SELECT content FROM agent_messages WHERE request_id = ? AND role = 'user' ORDER BY id ASC LIMIT 1`, requestID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return content, err
}

// FirstBuildPrompt is the earliest real user build prompt on a project,
// skipping incomplete-scaffold nudges.
func (r *Repo) FirstBuildPrompt(ctx context.Context, projectID int64) (string, error) {
	var content string
	err := r.db.QueryRowContext(ctx, `
		SELECT m.content FROM agent_messages m
		JOIN agent_requests ar ON ar.id = m.request_id
		JOIN agent_sessions s ON s.id = ar.session_id
		WHERE s.project_id = ? AND m.role = 'user'
		  AND m.content NOT LIKE 'You stopped before%'
		ORDER BY m.id ASC LIMIT 1`, projectID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return content, err
}

func (r *Repo) CompleteRequest(ctx context.Context, id int64, status string) error {
	// Settle any leftover streaming rows so nothing stays 'loading' forever.
	_, _ = r.db.ExecContext(ctx, `UPDATE agent_messages SET status='completed' WHERE request_id=? AND status='loading'`, id)
	_, err := r.db.ExecContext(ctx, `UPDATE agent_requests SET status=?, updated_at=CURRENT_TIMESTAMP, completed_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('streaming','waiting')`, status, id)
	return err
}

// RequestStatus is one agent_requests row plus its latest error line.
func (r *Repo) RequestStatus(ctx context.Context, id int64) (status, errMsg string, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT status FROM agent_requests WHERE id=?`, id).Scan(&status)
	if err != nil {
		return "", "", err
	}
	_ = r.db.QueryRowContext(ctx, `SELECT content FROM agent_messages WHERE request_id=? AND status='error' ORDER BY id DESC LIMIT 1`, id).Scan(&errMsg)
	return status, errMsg, nil
}

func (r *Repo) SetSessionModel(ctx context.Context, sessionID int64, model string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_sessions SET model=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, model, sessionID)
	return err
}

func (r *Repo) SetPiSessionID(ctx context.Context, sessionID int64, piSessionID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_sessions SET pi_session_id=? WHERE id=?`, piSessionID, sessionID)
	return err
}

func (r *Repo) InsertQuestion(ctx context.Context, projectID, requestID int64, rpcID, prompt, optionsJSON string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO agent_pending_questions (project_id,request_id,prompt,options_json,status,rpc_id) VALUES (?,?,?,?,'pending',?)`, projectID, requestID, prompt, optionsJSON, rpcID)
	return err
}

func (r *Repo) InsertPermission(ctx context.Context, projectID, requestID int64, rpcID, name, reason string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO agent_permission_requests (project_id,request_id,permission_name,reason,status,rpc_id) VALUES (?,?,?,?,'pending',?)`, projectID, requestID, name, reason, rpcID)
	return err
}

func (r *Repo) GetQuestion(ctx context.Context, id int64) (PendingQuestion, error) {
	var q PendingQuestion
	err := r.db.QueryRowContext(ctx, `SELECT id,project_id,request_id,prompt,options_json,status,rpc_id,created_at FROM agent_pending_questions WHERE id=?`, id).Scan(&q.ID, &q.ProjectID, &q.RequestID, &q.Prompt, &q.OptionsJSON, &q.Status, &q.RPCID, &q.CreatedAt)
	return q, err
}

func (r *Repo) GetPermission(ctx context.Context, id int64) (PermissionRequest, error) {
	var p PermissionRequest
	err := r.db.QueryRowContext(ctx, `SELECT id,project_id,request_id,permission_name,reason,status,rpc_id,created_at FROM agent_permission_requests WHERE id=?`, id).Scan(&p.ID, &p.ProjectID, &p.RequestID, &p.PermissionName, &p.Reason, &p.Status, &p.RPCID, &p.CreatedAt)
	return p, err
}

func (r *Repo) CancelRequest(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_requests SET status='cancelled', updated_at=CURRENT_TIMESTAMP, completed_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	return err
}
func (r *Repo) ResolveQuestion(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_pending_questions SET status=? WHERE id=?`, status, id)
	return err
}
func (r *Repo) ResolvePermission(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_permission_requests SET status=? WHERE id=?`, status, id)
	return err
}
func (r *Repo) PendingQuestion(ctx context.Context, projectID int64) (*PendingQuestion, error) {
	var q PendingQuestion
	err := r.db.QueryRowContext(ctx, `SELECT id,project_id,request_id,prompt,options_json,status,rpc_id,created_at FROM agent_pending_questions WHERE project_id=? AND status='pending' ORDER BY id DESC LIMIT 1`, projectID).Scan(&q.ID, &q.ProjectID, &q.RequestID, &q.Prompt, &q.OptionsJSON, &q.Status, &q.RPCID, &q.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err == nil {
		_ = json.Unmarshal([]byte(q.OptionsJSON), &q.Options)
	}
	return &q, err
}
func (r *Repo) PendingPermission(ctx context.Context, projectID int64) (*PermissionRequest, error) {
	var p PermissionRequest
	err := r.db.QueryRowContext(ctx, `SELECT id,project_id,request_id,permission_name,reason,status,rpc_id,created_at FROM agent_permission_requests WHERE project_id=? AND status='pending' ORDER BY id DESC LIMIT 1`, projectID).Scan(&p.ID, &p.ProjectID, &p.RequestID, &p.PermissionName, &p.Reason, &p.Status, &p.RPCID, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}
func (r *Repo) GetDraft(ctx context.Context, projectID int64) (PublishDraft, error) {
	var d PublishDraft
	var org sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT project_id,app_name,headline,description,changelog,publish_target,visibility,screenshot_manifest,expires_days,auto_install,organization_id,saved_at FROM publish_drafts WHERE project_id=?`, projectID).Scan(&d.ProjectID, &d.AppName, &d.Headline, &d.Description, &d.Changelog, &d.PublishTarget, &d.Visibility, &d.ScreenshotManifest, &d.ExpiresDays, &d.AutoInstall, &org, &d.SavedAt)
	if org.Valid {
		d.OrganizationID = &org.Int64
	}
	return d, err
}
func (r *Repo) SaveDraft(ctx context.Context, d PublishDraft) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO publish_drafts (project_id,app_name,headline,description,changelog,publish_target,visibility,screenshot_manifest,expires_days,auto_install,organization_id,saved_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(project_id) DO UPDATE SET app_name=excluded.app_name,headline=excluded.headline,description=excluded.description,changelog=excluded.changelog,publish_target=excluded.publish_target,visibility=excluded.visibility,screenshot_manifest=excluded.screenshot_manifest,expires_days=excluded.expires_days,auto_install=excluded.auto_install,organization_id=excluded.organization_id,saved_at=CURRENT_TIMESTAMP`, d.ProjectID, d.AppName, d.Headline, d.Description, d.Changelog, d.PublishTarget, d.Visibility, d.ScreenshotManifest, d.ExpiresDays, d.AutoInstall, r.actor.OrgID)
	return err
}
func (r *Repo) CreateJob(ctx context.Context, projectID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO publishing_jobs (project_id,status,error_message) VALUES (?,'queued','')`, projectID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SweepOrphanedJobs fails any job left queued/running by a previous
// process: their goroutines died with it, so the rows can never settle.
func (r *Repo) SweepOrphanedJobs(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE publishing_jobs SET status='failed', error_message='oozie quit while this build was running — publish again', updated_at=CURRENT_TIMESTAMP WHERE status IN ('queued','running')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *Repo) SetJobRunning(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE publishing_jobs SET status='running', updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	return err
}
func (r *Repo) FinishJob(ctx context.Context, id int64, status, errMsg string, storeAppID *int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE publishing_jobs SET status=?, error_message=?, store_app_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, errMsg, storeAppID, id)
	return err
}
func (r *Repo) UpsertStoreApp(ctx context.Context, projectID int64, d PublishDraft, artifactPath, slug string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM store_apps WHERE project_id=?`, projectID).Scan(&id)
	expiry := `CASE WHEN CAST(? AS INTEGER) > 0 THEN datetime('now', '+' || ? || ' days') ELSE NULL END`
	if err == sql.ErrNoRows {
		res, err := r.db.ExecContext(ctx, `INSERT INTO store_apps (project_id,organization_id,name,headline,description,visibility,published_version,last_published_at,artifact_path,bundle_slug,expires_at) VALUES (?,?,?,?,?,?,'1.0.0',CURRENT_TIMESTAMP,?,?,`+expiry+`)`, projectID, r.actor.OrgID, d.AppName, d.Headline, d.Description, d.Visibility, artifactPath, slug, d.ExpiresDays, d.ExpiresDays)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE store_apps SET name=?, headline=?, description=?, visibility=?, last_published_at=CURRENT_TIMESTAMP, artifact_path=?, bundle_slug=?, expires_at=`+expiry+` WHERE id=?`, d.AppName, d.Headline, d.Description, d.Visibility, artifactPath, slug, d.ExpiresDays, d.ExpiresDays, id)
	return id, err
}

// ExpiredAppIDs lists disposable apps whose time has come.
func (r *Repo) ExpiredAppIDs(ctx context.Context) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM store_apps WHERE expires_at IS NOT NULL AND expires_at <= datetime('now')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UserPrompts returns every user message ever sent to a project's agent,
// oldest first — the app's genome, for recipe export.
func (r *Repo) UserPrompts(ctx context.Context, projectID int64) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT m.content FROM agent_messages m JOIN agent_requests ar ON ar.id=m.request_id JOIN agent_sessions s ON s.id=ar.session_id WHERE s.project_id=? AND m.role='user' ORDER BY m.created_at, m.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StoreAppSlugForProject returns the published slug for a project, or ""
// when the project has never been published.
func (r *Repo) StoreAppSlugForProject(ctx context.Context, projectID int64) (string, error) {
	var slug string
	err := r.db.QueryRowContext(ctx, `SELECT bundle_slug FROM store_apps WHERE project_id=?`, projectID).Scan(&slug)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return slug, err
}

func (r *Repo) InsertImproveRequest(ctx context.Context, requestID, storeAppID int64, note string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO improve_requests (request_id,store_app_id,status,note) VALUES (?,?,'building',?)`, requestID, storeAppID, note)
	return err
}

func (r *Repo) ImproveByRequest(ctx context.Context, requestID int64) (*ImproveRequest, error) {
	var imp ImproveRequest
	err := r.db.QueryRowContext(ctx, `SELECT id,request_id,store_app_id,status,note,created_at FROM improve_requests WHERE request_id=?`, requestID).Scan(&imp.ID, &imp.RequestID, &imp.StoreAppID, &imp.Status, &imp.Note, &imp.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &imp, nil
}

func (r *Repo) SetImproveStatus(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE improve_requests SET status=? WHERE id=?`, status, id)
	return err
}

// RecordAppEvent stores a beacon event for the app with the given slug.
// Unknown slugs are a no-op, not an error.
func (r *Repo) RecordAppEvent(ctx context.Context, slug, kind string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO app_events (store_app_id, kind) SELECT id, ? FROM store_apps WHERE bundle_slug=?`, kind, slug)
	return err
}

// LatestJob is the newest publish attempt for a project.
func (r *Repo) LatestJob(ctx context.Context, projectID int64) (PublishingJob, error) {
	var j PublishingJob
	err := r.db.QueryRowContext(ctx, `SELECT id, project_id, status, error_message FROM publishing_jobs WHERE project_id=? ORDER BY id DESC LIMIT 1`, projectID).Scan(&j.ID, &j.ProjectID, &j.Status, &j.ErrorMessage)
	return j, err
}

// LatestRequest is the newest agent request for a project, plus its last error line.
func (r *Repo) LatestRequest(ctx context.Context, projectID int64) (status, errMsg string, err error) {
	var requestID int64
	err = r.db.QueryRowContext(ctx, `SELECT r.id, r.status FROM agent_requests r JOIN agent_sessions s ON s.id = r.session_id WHERE s.project_id=? ORDER BY r.id DESC LIMIT 1`, projectID).Scan(&requestID, &status)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	_ = r.db.QueryRowContext(ctx, `SELECT content FROM agent_messages WHERE request_id=? AND status='error' ORDER BY id DESC LIMIT 1`, requestID).Scan(&errMsg)
	return status, errMsg, nil
}

// LatestActivity is the newest agent line for a project, used by the waiting screen.
func (r *Repo) LatestActivity(ctx context.Context, projectID int64) (role, status, content string, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT m.role, m.status, m.content FROM agent_messages m JOIN agent_requests r ON r.id=m.request_id JOIN agent_sessions s ON s.id=r.session_id WHERE s.project_id=? ORDER BY m.id DESC LIMIT 1`, projectID).Scan(&role, &status, &content)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	return role, status, content, err
}

func (r *Repo) ListJobs(ctx context.Context, status string) ([]PublishingJob, error) {
	q := `SELECT j.id,j.project_id,p.name,j.store_app_id,j.status,j.error_message,j.created_at,j.updated_at FROM publishing_jobs j JOIN projects p ON p.id=j.project_id`
	args := []any{}
	if status != "" {
		q += ` WHERE j.status=?`
		args = append(args, status)
	}
	q += ` ORDER BY j.updated_at DESC LIMIT 50`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublishingJob
	for rows.Next() {
		var j PublishingJob
		var app sql.NullInt64
		if err := rows.Scan(&j.ID, &j.ProjectID, &j.ProjectName, &app, &j.Status, &j.ErrorMessage, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		if app.Valid {
			j.StoreAppID = &app.Int64
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *Repo) ListStoreApps(ctx context.Context, q, filter string) ([]StoreApp, error) {
	where := []string{"1=1"}
	args := []any{}
	if q != "" {
		where = append(where, "(name LIKE ? OR headline LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if filter == "featured" {
		where = append(where, "featured=1")
	} else if filter == "public" {
		where = append(where, "visibility='public'")
	} else if filter == "org" {
		where = append(where, "organization_id IS NOT NULL")
	} else if filter == "installed" {
		where = append(where, "EXISTS(SELECT 1 FROM installed_apps i WHERE i.store_app_id=s.id)")
	}
	rows, err := r.db.QueryContext(ctx, storeAppSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY featured DESC, install_count DESC LIMIT 30`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoreApp
	for rows.Next() {
		s, err := scanStoreApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const storeAppSelect = `SELECT s.id,s.project_id,s.organization_id,s.name,s.headline,s.description,s.visibility,s.published_version,s.last_published_at,s.install_count,s.featured,s.artifact_path, EXISTS(SELECT 1 FROM installed_apps i WHERE i.store_app_id=s.id),s.created_at,s.bundle_slug,s.expires_at,(SELECT COUNT(*) FROM app_events e WHERE e.store_app_id=s.id AND e.kind='launch'),(SELECT MAX(e.created_at) FROM app_events e WHERE e.store_app_id=s.id AND e.kind='launch'),s.public_url,s.runtime_pid FROM store_apps s`

func (r *Repo) GetStoreApp(ctx context.Context, id int64) (StoreApp, error) {
	row := r.db.QueryRowContext(ctx, storeAppSelect+` WHERE s.id=?`, id)
	return scanStoreApp(row)
}

func (r *Repo) GetStoreAppBySlug(ctx context.Context, slug string) (StoreApp, error) {
	row := r.db.QueryRowContext(ctx, storeAppSelect+` WHERE s.bundle_slug=?`, slug)
	return scanStoreApp(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanStoreApp(row rowScanner) (StoreApp, error) {
	var s StoreApp
	var pid, oid sql.NullInt64
	var expires sql.NullTime
	// MAX(created_at) is an expression, so the driver returns it as a raw
	// string (no column type to map) — scan text and parse.
	var lastLaunch sql.NullString
	if err := row.Scan(&s.ID, &pid, &oid, &s.Name, &s.Headline, &s.Description, &s.Visibility, &s.PublishedVersion, &s.LastPublishedAt, &s.InstallCount, &s.Featured, &s.ArtifactPath, &s.Installed, &s.CreatedAt, &s.BundleSlug, &expires, &s.LaunchCount, &lastLaunch, &s.PublicURL, &s.RuntimePID); err != nil {
		return StoreApp{}, err
	}
	if expires.Valid {
		s.ExpiresAt = &expires.Time
	}
	if lastLaunch.Valid {
		if t, err := parseSQLiteTime(lastLaunch.String); err == nil {
			s.LastLaunchAt = &t
		}
	}
	if pid.Valid {
		s.ProjectID = &pid.Int64
	}
	if oid.Valid {
		s.OrganizationID = &oid.Int64
	}
	return s, nil
}
func (r *Repo) UninstallApp(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM installed_apps WHERE store_app_id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = r.db.ExecContext(ctx, `UPDATE store_apps SET install_count=MAX(install_count-1,0) WHERE id=?`, id)
	}
	return nil
}
func (r *Repo) DeleteStoreApp(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM store_apps WHERE id=?`, id)
	return err
}
func (r *Repo) SetRuntime(ctx context.Context, id int64, publicURL string, pid int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE store_apps SET public_url=?, runtime_pid=? WHERE id=?`, publicURL, pid, id)
	return err
}
func (r *Repo) InstallApp(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO installed_apps (store_app_id,user_id) VALUES (?,?)`, id, r.actor.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = r.db.ExecContext(ctx, `UPDATE store_apps SET install_count=install_count+1 WHERE id=?`, id)
	}
	return nil
}
func (r *Repo) InstalledApps(ctx context.Context) ([]StoreApp, error) {
	return r.ListStoreApps(ctx, "", "installed")
}

func (r *Repo) GetSettings(ctx context.Context) (Settings, error) {
	var s Settings
	err := r.db.QueryRowContext(ctx, `SELECT appearance,style_profile,fairy_enabled,fairy_hour FROM user_settings WHERE user_id=?`, r.actor.UserID).Scan(&s.Appearance, &s.StyleProfile, &s.FairyEnabled, &s.FairyHour)
	return s, err
}
func (r *Repo) SaveSettings(ctx context.Context, s Settings) error {
	_, err := r.db.ExecContext(ctx, `UPDATE user_settings SET appearance=?,style_profile=?,fairy_enabled=?,fairy_hour=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=?`, s.Appearance, s.StyleProfile, s.FairyEnabled, s.FairyHour, r.actor.UserID)
	return err
}

// IndustryPack is the empty slot a later pack can fill. Empty means no extra prompt text.
func (r *Repo) IndustryPack(ctx context.Context) string {
	var pack string
	_ = r.db.QueryRowContext(ctx, `SELECT industry_pack FROM organizations WHERE id=?`, r.actor.OrgID).Scan(&pack)
	return strings.TrimSpace(pack)
}

func (r *Repo) AddWish(ctx context.Context, text string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO wishes (text) VALUES (?)`, text)
	return err
}
func (r *Repo) DeleteWish(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM wishes WHERE id=?`, id)
	return err
}
func (r *Repo) GetWish(ctx context.Context, id int64) (Wish, error) {
	return scanWish(r.db.QueryRowContext(ctx, `SELECT id,text,status,project_id,error,created_at,built_at FROM wishes WHERE id=?`, id))
}
func (r *Repo) ListWishes(ctx context.Context) ([]Wish, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,text,status,project_id,error,created_at,built_at FROM wishes ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'building' THEN 1 ELSE 2 END, id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Wish
	for rows.Next() {
		w, err := scanWish(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func scanWish(row rowScanner) (Wish, error) {
	var w Wish
	var pid sql.NullInt64
	var built sql.NullTime
	if err := row.Scan(&w.ID, &w.Text, &w.Status, &pid, &w.Error, &w.CreatedAt, &built); err != nil {
		return Wish{}, err
	}
	if pid.Valid {
		w.ProjectID = &pid.Int64
	}
	if built.Valid {
		w.BuiltAt = &built.Time
	}
	return w, nil
}
func (r *Repo) PendingWishes(ctx context.Context, limit int) ([]Wish, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,text,status,project_id,error,created_at,built_at FROM wishes WHERE status='pending' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Wish
	for rows.Next() {
		w, err := scanWish(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (r *Repo) SetWishBuilding(ctx context.Context, id, projectID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE wishes SET status='building', project_id=? WHERE id=?`, projectID, id)
	return err
}
func (r *Repo) SettleWish(ctx context.Context, id int64, status, errMsg string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE wishes SET status=?, error=?, built_at=CURRENT_TIMESTAMP WHERE id=?`, status, errMsg, id)
	return err
}

// SweepStalePrompts expires questions and permission requests left
// pending by a dead process: no pi process survives a restart, so any
// pending prompt at startup can never be answered.
func (r *Repo) SweepStalePrompts(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE agent_pending_questions SET status='expired' WHERE status='pending'`); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE agent_permission_requests SET status='expired' WHERE status='pending'`)
	return err
}

// SweepStaleAgentRequests fails agent runs left streaming/waiting by a
// previous process and settles any loading message rows they left behind.
// Without this, the agent UI looks mid-flight forever after a crash/quit.
func (r *Repo) SweepStaleAgentRequests(ctx context.Context) (int64, error) {
	// Settle loading rows first while we can still identify the stranded
	// requests by status; then fail the requests themselves.
	_, _ = r.db.ExecContext(ctx, `UPDATE agent_messages SET status='completed' WHERE status='loading' AND request_id IN (
		SELECT id FROM agent_requests WHERE status IN ('streaming','waiting')
	)`)
	res, err := r.db.ExecContext(ctx, `UPDATE agent_requests SET status='failed', updated_at=CURRENT_TIMESTAMP, completed_at=CURRENT_TIMESTAMP WHERE status IN ('streaming','waiting')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepStaleWishes fails wishes left 'building' by a dead process.
func (r *Repo) SweepStaleWishes(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE wishes SET status='failed', error='oozie quit while this wish was building — set it back to pending or build it now' WHERE status='building'`)
	return err
}

func (r *Repo) CreateRecipeDraft(ctx context.Context, d RecipeDraft) (int64, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO recipe_drafts
		(source_url, source_kind, name, headline, store_description, plan, recipe_json, status)
		VALUES (?,?,?,?,?,?,?,?)`,
		d.SourceURL, d.SourceKind, d.Name, d.Headline, d.StoreDescription, d.Plan, d.RecipeJSON, d.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repo) GetRecipeDraft(ctx context.Context, id int64) (RecipeDraft, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, source_url, source_kind, name, headline, store_description,
		plan, recipe_json, status, project_id, created_at, updated_at FROM recipe_drafts WHERE id=?`, id)
	return scanRecipeDraft(row)
}

func (r *Repo) UpdateRecipeDraftPlan(ctx context.Context, id int64, plan, recipeJSON string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE recipe_drafts SET plan=?, recipe_json=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='pending'`,
		plan, recipeJSON, id)
	return err
}

func (r *Repo) SettleRecipeDraft(ctx context.Context, id int64, status string, projectID *int64) error {
	var pid any
	if projectID != nil {
		pid = *projectID
	}
	_, err := r.db.ExecContext(ctx, `UPDATE recipe_drafts SET status=?, project_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		status, pid, id)
	return err
}

func scanRecipeDraft(row rowScanner) (RecipeDraft, error) {
	var d RecipeDraft
	var pid sql.NullInt64
	var created, updated string
	if err := row.Scan(&d.ID, &d.SourceURL, &d.SourceKind, &d.Name, &d.Headline, &d.StoreDescription,
		&d.Plan, &d.RecipeJSON, &d.Status, &pid, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return RecipeDraft{}, ErrValidation{"That recipe draft is gone."}
		}
		return RecipeDraft{}, err
	}
	if pid.Valid {
		d.ProjectID = &pid.Int64
	}
	if t, err := parseSQLiteTime(created); err == nil {
		d.CreatedAt = t
	}
	if t, err := parseSQLiteTime(updated); err == nil {
		d.UpdatedAt = t
	}
	return d, nil
}
