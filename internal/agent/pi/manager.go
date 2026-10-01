package pi

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// approvalExtension gates mutating tool calls behind a confirm dialog for
// untrusted projects. pi's --no-approve only ignores project-local config;
// it does not restrict tools, so this extension is the actual enforcement.
//
//go:embed approval.ts
var approvalExtension []byte

// Sink receives agent activity so the caller can persist it. Calls arrive
// from the process reader goroutine, never concurrently for one project.
type Sink interface {
	AssistantMessage(projectID, requestID int64, content string)
	// AssistantPartial delivers the text-so-far of a streaming assistant
	// message (throttled); the final AssistantMessage supersedes it.
	AssistantPartial(projectID, requestID int64, content string)
	// ToolStarted announces a tool call beginning; ToolFinished replaces it
	// (matched by callID) with the outcome and an expandable body — the
	// file content written or the command output.
	ToolStarted(projectID, requestID int64, callID, content string)
	ToolFinished(projectID, requestID int64, callID, content, body string)
	RequestSettled(projectID, requestID int64, status string)
	Question(projectID, requestID int64, rpcID, prompt, optionsJSON string)
	Permission(projectID, requestID int64, rpcID, name, reason string)
	AgentError(projectID, requestID int64, message string)
}

// SessionStats mirrors pi's get_session_stats response data.
type SessionStats struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	Cost           float64
	ContextPercent float64
	ContextWindow  int64
}

// idleTimeout is how long a non-streaming pi process may sit unused before
// the reaper stops it. Sessions persist on disk (--session-id), so the next
// prompt transparently restarts the process with full context.
const idleTimeout = 30 * time.Minute

// Manager owns one pi RPC subprocess per project.
type Manager struct {
	mu          sync.Mutex
	procs       map[int64]*proc
	sink        Sink
	catalog     Catalog
	binary      string
	approvalExt string // path to the materialized approval extension
	stop        chan struct{}
}

func NewManager(catalog Catalog, sink Sink) *Manager {
	m := &Manager{procs: map[int64]*proc{}, sink: sink, catalog: catalog, binary: resolvePiBinary(), approvalExt: materializeApprovalExtension(), stop: make(chan struct{})}
	go m.reapLoop()
	return m
}

// resolvePiBinary finds the pi executable even when Oozie Desk was launched
// from Finder, where PATH is the bare system default and Homebrew paths
// are missing.
func resolvePiBinary() string {
	if v := os.Getenv("PI_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("pi"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/opt/homebrew/bin/pi",
		"/usr/local/bin/pi",
		filepath.Join(home, ".local", "bin", "pi"),
		filepath.Join(home, "bin", "pi"),
		filepath.Join(home, ".npm-global", "bin", "pi"),
	}
	// Version-managed installs relocate on every update
	// (~/.local/share/pi-node/node-vX.Y.Z-darwin-x64/bin/pi), so match by
	// glob — newest version last, hence the reverse walk below.
	if globbed, err := filepath.Glob(filepath.Join(home, ".local", "share", "pi-node", "*", "bin", "pi")); err == nil {
		sort.Strings(globbed)
		for i := len(globbed) - 1; i >= 0; i-- {
			candidates = append(candidates, globbed[i])
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	// Last resort: ask the login shell, which has the PATH from rc files
	// that a Finder-launched app never inherits.
	if out, err := exec.Command("/bin/zsh", "-lc", "command -v pi").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return "pi"
}

// augmentedPathEnv returns the process environment with the common
// Homebrew/local bin directories — plus any extra dirs, e.g. pi's own bin
// dir so its sibling node binary resolves — appended to PATH, so pi's own
// bash tool finds developer tooling under a Finder-launched Oozie Desk.
func augmentedPathEnv(binDirs ...string) []string {
	env := os.Environ()
	extras := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	for _, d := range binDirs {
		if d != "" && d != "." {
			extras = append(extras, d)
		}
	}
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path := strings.TrimPrefix(kv, "PATH=")
			for _, extra := range extras {
				if !strings.Contains(":"+path+":", ":"+extra+":") {
					path += ":" + extra
				}
			}
			env[i] = "PATH=" + path
			return env
		}
	}
	return append(env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:"+strings.Join(extras, ":"))
}

// reapLoop stops pi processes that have been idle past idleTimeout.
func (m *Manager) reapLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		for id, p := range m.procs {
			p.mu.Lock()
			idle := !p.streaming && time.Since(p.lastActive) > idleTimeout
			p.mu.Unlock()
			if idle {
				log.Printf("pi project %d: stopping idle process (session persists)", id)
				p.stop()
				delete(m.procs, id)
			}
		}
		m.mu.Unlock()
	}
}

// Stats returns the last known session stats for the project's agent, or
// nil if none have been collected yet (or the process isn't running).
func (m *Manager) Stats(projectID int64) *SessionStats {
	p := m.get(projectID)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// materializeApprovalExtension writes the embedded approval extension to a
// stable path so pi can load it via -e. Returns "" on failure, in which
// case untrusted projects refuse to start rather than run ungated.
func materializeApprovalExtension() string {
	path := filepath.Join(os.TempDir(), "oozie-approval-extension.ts")
	if err := os.WriteFile(path, approvalExtension, 0o644); err != nil {
		log.Printf("pi: cannot write approval extension: %v", err)
		return ""
	}
	return path
}

// StartOptions describe how to launch pi for a project.
// Turn is one prior user or assistant message for the next prompt.
type Turn struct {
	Role    string
	Content string
}

type StartOptions struct {
	ProjectID    int64
	Workdir      string
	Model        string // "provider/id", may be empty for pi's default
	PiSessionID  string
	SystemPrompt string
	Trusted      bool
	History      []Turn
}

// Prompt ensures a pi process is running for the project and sends the
// message. Events stream back through the Sink asynchronously.
func (m *Manager) Prompt(opts StartOptions, requestID int64, message string) error {
	p, err := m.ensure(opts)
	if err != nil {
		return err
	}
	return p.prompt(requestID, message, opts.Model)
}

// StopProject terminates the project's pi process if one is running
// (used when a project is deleted; the on-disk session is untouched).
func (m *Manager) StopProject(projectID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[projectID]; ok {
		p.stop()
		delete(m.procs, projectID)
	}
}

func (m *Manager) Abort(projectID int64) error {
	if p := m.get(projectID); p != nil {
		return p.send(map[string]any{"type": "abort"})
	}
	return nil
}

// SetModel switches the model on a running process. If no process is
// running, this is a no-op: the model is applied at next launch.
func (m *Manager) SetModel(projectID int64, model string) error {
	p := m.get(projectID)
	if p == nil {
		return nil
	}
	return p.setModel(model)
}

func (m *Manager) RespondValue(projectID int64, rpcID, value string) error {
	if p := m.get(projectID); p != nil {
		return p.send(map[string]any{"type": "extension_ui_response", "id": rpcID, "value": value})
	}
	return fmt.Errorf("agent is not running")
}

func (m *Manager) RespondConfirm(projectID int64, rpcID string, confirmed bool) error {
	if p := m.get(projectID); p != nil {
		return p.send(map[string]any{"type": "extension_ui_response", "id": rpcID, "confirmed": confirmed})
	}
	return fmt.Errorf("agent is not running")
}

func (m *Manager) RespondCancel(projectID int64, rpcID string) error {
	if p := m.get(projectID); p != nil {
		return p.send(map[string]any{"type": "extension_ui_response", "id": rpcID, "cancelled": true})
	}
	return fmt.Errorf("agent is not running")
}

// Streaming reports whether the project's agent is currently working.
func (m *Manager) Streaming(projectID int64) bool {
	if p := m.get(projectID); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.streaming
	}
	return false
}

func (m *Manager) Shutdown() {
	close(m.stop)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.procs {
		p.stop()
		delete(m.procs, id)
	}
}

func (m *Manager) get(projectID int64) *proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[projectID]
}

func (m *Manager) ensure(opts StartOptions) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[opts.ProjectID]; ok && p.alive() {
		return p, nil
	}

	args := []string{"--mode", "rpc"}
	if opts.PiSessionID != "" {
		args = append(args, "--session-id", opts.PiSessionID)
	}
	if opt, ok := splitModel(opts.Model); ok {
		args = append(args, "--provider", opt.Provider, "--model", opt.ID)
	}
	if m.catalog.ThinkingLevel != "" {
		args = append(args, "--thinking", m.catalog.ThinkingLevel)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.SystemPrompt)
	}
	if opts.Trusted {
		args = append(args, "--approve")
	} else {
		// --no-approve only ignores project-local config files; the
		// approval extension is what gates write/edit/bash behind the
		// permission panel. Without it an untrusted project would run
		// fully unrestricted, so fail closed instead.
		if m.approvalExt == "" {
			return nil, fmt.Errorf("approval extension unavailable; refusing to start agent for untrusted project")
		}
		args = append(args, "--no-approve", "-e", m.approvalExt)
	}

	cmd := exec.Command(m.binary, args...)
	cmd.Dir = opts.Workdir
	cmd.Env = augmentedPathEnv(filepath.Dir(m.binary))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi (%s): %w", m.binary, err)
	}

	p := &proc{
		projectID:  opts.ProjectID,
		cmd:        cmd,
		stdin:      stdin,
		sink:       m.sink,
		model:      opts.Model,
		done:       make(chan struct{}),
		lastActive: time.Now(),
	}
	m.procs[opts.ProjectID] = p
	go p.readLoop(stdout)
	return p, nil
}

type proc struct {
	projectID int64
	cmd       *exec.Cmd
	sink      Sink

	mu               sync.Mutex
	stdin            io.WriteCloser
	model            string
	streaming        bool
	hadError         bool
	currentRequestID int64
	done             chan struct{}
	exited           bool
	lastActive       time.Time
	lastPartialFlush time.Time
	lastPartialLen   int
	stats            *SessionStats
	// toolArgs remembers each call's args from tool_execution_start,
	// because pi's tool_execution_end event doesn't repeat them.
	toolArgs map[string]map[string]any
}

func (p *proc) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.exited
}

func (p *proc) stop() {
	_ = p.send(map[string]any{"type": "abort"})
	p.mu.Lock()
	stdin := p.stdin
	p.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *proc) send(cmd map[string]any) error {
	body, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return fmt.Errorf("agent process has exited")
	}
	_, err = p.stdin.Write(append(body, '\n'))
	return err
}

func (p *proc) setModel(model string) error {
	opt, ok := splitModel(model)
	if !ok {
		return fmt.Errorf("invalid model %q", model)
	}
	p.mu.Lock()
	same := p.model == model
	p.mu.Unlock()
	if same {
		return nil
	}
	if err := p.send(map[string]any{"type": "set_model", "provider": opt.Provider, "modelId": opt.ID}); err != nil {
		return err
	}
	p.mu.Lock()
	p.model = model
	p.mu.Unlock()
	return nil
}

func (p *proc) prompt(requestID int64, message, model string) error {
	if model != "" {
		if err := p.setModel(model); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.currentRequestID = requestID
	p.lastActive = time.Now()
	p.lastPartialLen = 0
	busy := p.streaming
	p.mu.Unlock()

	cmd := map[string]any{"type": "prompt", "message": message}
	if busy {
		cmd["streamingBehavior"] = "steer"
	}
	return p.send(cmd)
}

// event is a loosely-typed view over pi's RPC event stream.
type event struct {
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`

	// Message is an object for message_* events but a plain string for
	// extension_ui_request, so it stays raw and is decoded per event type.
	Message json.RawMessage `json:"message"`

	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	Args       map[string]any  `json:"args"`
	IsError    bool            `json:"isError"`
	Result     json.RawMessage `json:"result"`

	// extension_ui_request payloads are decoded separately in
	// handleUIRequest because their "message" field is a string and
	// would conflict with Message above.
}

func (p *proc) readLoop(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 1024*1024)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line != "" {
			p.handleLine(line)
		}
		if err != nil {
			break
		}
	}
	_ = p.cmd.Wait()

	p.mu.Lock()
	p.exited = true
	requestID := p.currentRequestID
	wasStreaming := p.streaming
	p.streaming = false
	p.currentRequestID = 0
	close(p.done)
	p.mu.Unlock()

	if wasStreaming && requestID != 0 {
		p.sink.AgentError(p.projectID, requestID, "The pi agent process exited unexpectedly.")
		p.sink.RequestSettled(p.projectID, requestID, "failed")
	}
	log.Printf("pi process for project %d exited", p.projectID)
}

func (p *proc) handleLine(line string) {
	var ev event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		log.Printf("pi project %d: unparseable event: %.200s", p.projectID, line)
		return
	}

	p.mu.Lock()
	requestID := p.currentRequestID
	p.lastActive = time.Now()
	p.mu.Unlock()

	switch ev.Type {
	case "agent_start":
		p.mu.Lock()
		p.streaming = true
		p.mu.Unlock()

	case "message_update":
		p.handlePartial(ev, requestID)

	case "message_end":
		var msg struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
		}
		if len(ev.Message) > 0 && json.Unmarshal(ev.Message, &msg) == nil && msg.Role == "assistant" && requestID != 0 {
			if text := extractText(msg.Content); text != "" {
				p.sink.AssistantMessage(p.projectID, requestID, text)
			}
			if msg.StopReason == "error" || msg.StopReason == "aborted" {
				p.mu.Lock()
				p.hadError = true
				p.mu.Unlock()
				detail := msg.ErrorMessage
				if detail == "" {
					detail = "The model run was " + msg.StopReason + "."
				}
				p.sink.AgentError(p.projectID, requestID, detail)
			}
		}

	case "tool_execution_start":
		p.mu.Lock()
		if p.toolArgs == nil {
			p.toolArgs = map[string]map[string]any{}
		}
		p.toolArgs[ev.ToolCallID] = ev.Args
		p.mu.Unlock()
		if requestID != 0 {
			p.sink.ToolStarted(p.projectID, requestID, ev.ToolCallID, describeTool(ev.ToolName, ev.Args, "running"))
		}

	case "tool_execution_end":
		p.mu.Lock()
		if args := p.toolArgs[ev.ToolCallID]; args != nil {
			ev.Args = args
			delete(p.toolArgs, ev.ToolCallID)
		}
		p.mu.Unlock()
		if requestID != 0 {
			status := "ok"
			if ev.IsError {
				status = "error"
			}
			p.sink.ToolFinished(p.projectID, requestID, ev.ToolCallID, describeTool(ev.ToolName, ev.Args, status), toolBody(ev))
		}

	case "agent_settled":
		p.mu.Lock()
		p.streaming = false
		p.currentRequestID = 0
		p.lastPartialLen = 0
		status := "completed"
		if p.hadError {
			status = "failed"
			p.hadError = false
		}
		p.mu.Unlock()
		if requestID != 0 {
			p.sink.RequestSettled(p.projectID, requestID, status)
		}
		// Refresh session stats after each run; the response is handled below.
		_ = p.send(map[string]any{"type": "get_session_stats"})

	case "extension_ui_request":
		p.handleUIRequest(line, requestID)

	case "response":
		if ev.Success != nil && !*ev.Success && ev.Command == "prompt" && requestID != 0 {
			p.sink.AgentError(p.projectID, requestID, "pi rejected the prompt: "+ev.Error)
			p.sink.RequestSettled(p.projectID, requestID, "failed")
			p.mu.Lock()
			p.currentRequestID = 0
			p.mu.Unlock()
		}
		if ev.Command == "get_session_stats" && ev.Success != nil && *ev.Success {
			p.handleStats(ev.Data)
		}
	}
}

// handlePartial persists the text-so-far of a streaming assistant message,
// throttled so rapid token deltas don't hammer the database.
func (p *proc) handlePartial(ev event, requestID int64) {
	if requestID == 0 || len(ev.Message) == 0 {
		return
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(ev.Message, &msg) != nil || msg.Role != "assistant" {
		return
	}
	text := extractText(msg.Content)
	if text == "" {
		return
	}
	p.mu.Lock()
	grown := len(text) > p.lastPartialLen
	due := time.Since(p.lastPartialFlush) >= 400*time.Millisecond
	if grown && due {
		p.lastPartialLen = len(text)
		p.lastPartialFlush = time.Now()
	}
	p.mu.Unlock()
	if grown && due {
		p.sink.AssistantPartial(p.projectID, requestID, text)
	}
}

func (p *proc) handleStats(data json.RawMessage) {
	var d struct {
		Tokens struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
			Total  int64 `json:"total"`
		} `json:"tokens"`
		Cost         float64 `json:"cost"`
		ContextUsage struct {
			ContextWindow int64   `json:"contextWindow"`
			Percent       float64 `json:"percent"`
		} `json:"contextUsage"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	p.mu.Lock()
	p.stats = &SessionStats{
		InputTokens:    d.Tokens.Input,
		OutputTokens:   d.Tokens.Output,
		TotalTokens:    d.Tokens.Total,
		Cost:           d.Cost,
		ContextPercent: d.ContextUsage.Percent,
		ContextWindow:  d.ContextUsage.ContextWindow,
	}
	p.mu.Unlock()
}

func (p *proc) handleUIRequest(line string, requestID int64) {
	var req struct {
		ID      string   `json:"id"`
		Method  string   `json:"method"`
		Title   string   `json:"title"`
		Message string   `json:"message"`
		Options []string `json:"options"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return
	}
	switch req.Method {
	case "select", "input", "editor":
		options := "[]"
		if len(req.Options) > 0 {
			if b, err := json.Marshal(req.Options); err == nil {
				options = string(b)
			}
		}
		p.sink.Question(p.projectID, requestID, req.ID, req.Title, options)
	case "confirm":
		p.sink.Permission(p.projectID, requestID, req.ID, req.Title, req.Message)
	default:
		// notify/setStatus/setWidget/etc. are fire-and-forget; ignore.
	}
}

// extractText joins the text blocks of an assistant message. Content is
// either a plain string or an array of typed blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func describeTool(name string, args map[string]any, status string) string {
	detail := ""
	for _, key := range []string{"command", "path", "file_path", "pattern", "url"} {
		if v, ok := args[key].(string); ok && v != "" {
			detail = v
			break
		}
	}
	if len(detail) > 120 {
		detail = detail[:117] + "…"
	}
	if detail == "" {
		return fmt.Sprintf("%s (%s)", name, status)
	}
	return fmt.Sprintf("%s: %s (%s)", name, detail, status)
}

const maxToolBody = 6000

// toolBody extracts the interesting payload of a finished tool call: the
// code written (write/edit) or the tool's output (bash and friends).
func toolBody(ev event) string {
	body := ""
	switch ev.ToolName {
	case "write":
		body, _ = ev.Args["content"].(string)
	case "edit":
		for _, key := range []string{"newText", "new_text", "new_string", "newStr", "replacement"} {
			if v, ok := ev.Args[key].(string); ok && v != "" {
				body = v
				break
			}
		}
	}
	if body == "" && len(ev.Result) > 0 {
		var res struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(ev.Result, &res) == nil {
			var parts []string
			for _, c := range res.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, c.Text)
				}
			}
			body = strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	if len(body) > maxToolBody {
		body = body[:maxToolBody] + "\n… (truncated)"
	}
	return body
}
