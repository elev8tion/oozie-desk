package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"oozie-desk/internal/agent/pi"
)

const (
	maxRounds       = 48
	maxCutOffRounds = 2
	idleTimeout     = 30 * time.Minute
	defaultSystem   = "You are Oozie Desk's coding agent. Build one small Go web tool: one page, one job. A second .go file in the same package is fine if one write would be cut off. No canvas, PDF engine, or second app. Write each file in one complete tool call. A cut-off write is a failure. Prefer read/ls before write. Use bash for go build."
	cutOffReply     = "the model reply was cut off before the file was written"
)

// Manager runs in-process LLM tool loops. Same surface as pi.Manager for the desk.
type Manager struct {
	catalog pi.Catalog
	sink    pi.Sink
	client  *ChatClient

	mu       sync.Mutex
	sessions map[int64]*session
}

type session struct {
	opts     pi.StartOptions
	cancel   context.CancelFunc
	gen      uint64 // bumps each Prompt; run cleanup only touches matching gen
	busy     bool
	stats    pi.SessionStats
	pending  map[string]chan bool // permission rpcID -> answer
	question map[string]chan string
}

// NewManager builds a desk-owned agent. keys may be empty (loaded per call).
func NewManager(catalog pi.Catalog, sink pi.Sink, keys Keys) *Manager {
	return &Manager{
		catalog:  MergeCatalog(catalog),
		sink:     sink,
		client:   &ChatClient{Keys: keys},
		sessions: map[int64]*session{},
	}
}

// RefreshKeys reloads credentials from env + auth file.
func (m *Manager) RefreshKeys() {
	m.client.Keys = LoadKeys()
}

// CompleteText runs a single-turn, no-tools completion (recipe draft plans).
func (m *Manager) CompleteText(ctx context.Context, model, system, user string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("model required")
	}
	if _, _, _, err := m.client.Keys.ResolveEndpoint(model); err != nil {
		m.client.Keys = LoadKeys()
		if _, _, _, err2 := m.client.Keys.ResolveEndpoint(model); err2 != nil {
			return "", err2
		}
	}
	return m.client.CompleteText(ctx, model, system, user)
}

// Prompt starts or continues a project session with one user message.
func (m *Manager) Prompt(opts pi.StartOptions, requestID int64, message string) error {
	if opts.ProjectID == 0 {
		return fmt.Errorf("project id required")
	}
	if strings.TrimSpace(opts.Workdir) == "" {
		return fmt.Errorf("workdir required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return fmt.Errorf("model required")
	}
	// Fail fast if no key for this model.
	if _, _, _, err := m.client.Keys.ResolveEndpoint(opts.Model); err != nil {
		m.client.Keys = LoadKeys()
		if _, _, _, err2 := m.client.Keys.ResolveEndpoint(opts.Model); err2 != nil {
			return err2
		}
	}

	m.mu.Lock()
	s := m.sessions[opts.ProjectID]
	if s != nil && s.busy {
		m.mu.Unlock()
		return fmt.Errorf("agent already running for this project")
	}
	if s == nil {
		s = &session{pending: map[string]chan bool{}, question: map[string]chan string{}}
		m.sessions[opts.ProjectID] = s
	}
	s.opts = opts
	ctx, cancel := context.WithTimeout(context.Background(), idleTimeout)
	s.cancel = cancel
	s.gen++
	myGen := s.gen
	s.busy = true
	m.mu.Unlock()

	go m.run(ctx, opts.ProjectID, requestID, message, myGen)
	return nil
}

func promptMessages(sys, userMsg string, history []pi.Turn) []chatMessage {
	messages := []chatMessage{{Role: "system", Content: sys}}
	for _, turn := range history {
		role := turn.Role
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(turn.Content)
		if content == "" {
			continue
		}
		if len(content) > 2000 {
			content = content[:2000]
		}
		messages = append(messages, chatMessage{Role: role, Content: content})
	}
	messages = append(messages, chatMessage{Role: "user", Content: userMsg})
	return messages
}

func (m *Manager) run(ctx context.Context, projectID, requestID int64, userMsg string, gen uint64) {
	m.mu.Lock()
	s := m.sessions[projectID]
	opts := s.opts
	m.mu.Unlock()

	// Only clear busy/cancel if we still own the session slot. RequestSettled may
	// start a credit retry that bumps gen; touching that session would kill the
	// retry mid-flight (failed request with only the user message).
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		s := m.sessions[projectID]
		if s == nil || s.gen != gen {
			return
		}
		s.busy = false
		s.cancel = nil
	}
	defer release()

	sys := strings.TrimSpace(opts.SystemPrompt)
	if sys == "" {
		sys = defaultSystem
	}
	messages := promptMessages(sys, userMsg, opts.History)

	var lastErr error
	cutOffs := 0
	for round := 0; round < maxRounds; round++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		msg, usage, err := m.client.Complete(ctx, opts.Model, messages)
		if err != nil {
			lastErr = err
			if m.sink != nil {
				m.sink.AgentError(projectID, requestID, err.Error())
			}
			break
		}
		if usage != nil {
			m.mu.Lock()
			s.stats.InputTokens += usage.Input
			s.stats.OutputTokens += usage.Output
			s.stats.TotalTokens += usage.Total
			m.mu.Unlock()
		}

		if len(msg.ToolCalls) == 0 {
			if msg.FinishReason == "length" {
				cutOffs++
				if cutOffs >= maxCutOffRounds {
					lastErr = fmt.Errorf("%s", cutOffReply)
					if m.sink != nil {
						m.sink.AgentError(projectID, requestID, lastErr.Error())
					}
					break
				}
				messages = append(messages, chatMessage{Role: "assistant", Content: msg.Content})
				messages = append(messages, chatMessage{Role: "user", Content: "Your last reply was cut off. Write the file in one complete call, or split it into a second .go file. Do not send a partial file."})
				continue
			}
			text := strings.TrimSpace(msg.Content)
			if text != "" && m.sink != nil {
				m.sink.AssistantMessage(projectID, requestID, text)
			}
			lastErr = nil
			break
		}

		broken := msg.FinishReason == "length" || callsBroken(msg.ToolCalls)
		if broken {
			cutOffs++
		}

		// Keep assistant turn with tool_calls for the API history.
		messages = append(messages, chatMessage{
			Role:      "assistant",
			Content:   msg.Content,
			ToolCalls: msg.ToolCalls,
		})

		for i, tc := range msg.ToolCalls {
			name := tc.Function.Name
			args := tc.Function.Arguments
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("%s-%d-%d", name, round, i)
			}
			if m.sink != nil {
				m.sink.ToolStarted(projectID, requestID, callID, name+": "+short(args, 80))
			}
			if !opts.Trusted && mutatingTool(name) {
				ok := m.awaitPermission(ctx, projectID, requestID, name, args)
				if !ok {
					out := "permission denied by user"
					if m.sink != nil {
						m.sink.ToolFinished(projectID, requestID, callID, name+" (denied)", out)
					}
					messages = append(messages, chatMessage{
						Role:       "tool",
						ToolCallID: callID,
						Name:       name,
						Content:    out,
					})
					continue
				}
			}
			if broken || toolCallBroken(tc) {
				body := "cut off: do not retry this call. Write a complete smaller file, or split into a second .go file."
				if m.sink != nil {
					m.sink.ToolFinished(projectID, requestID, callID, name+" (cut off)", body)
				}
				messages = append(messages, chatMessage{
					Role:       "tool",
					ToolCallID: callID,
					Name:       name,
					Content:    body,
				})
				continue
			}
			summary, body, toolErr := runTool(ctx, opts.Workdir, name, args)
			if toolErr != nil && (strings.Contains(body, "invalid tool arguments") || strings.Contains(body, "empty command")) {
				cutOffs++
				broken = true
			}
			if toolErr != nil && body == "" {
				body = toolErr.Error()
			}
			if m.sink != nil {
				m.sink.ToolFinished(projectID, requestID, callID, summary, body)
			}
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: callID,
				Name:       name,
				Content:    body,
			})
		}
		if broken && cutOffs >= maxCutOffRounds {
			lastErr = fmt.Errorf("%s", cutOffReply)
			if m.sink != nil {
				m.sink.AgentError(projectID, requestID, lastErr.Error())
			}
			break
		}
		if broken {
			messages = append(messages, chatMessage{Role: "user", Content: "That tool call was cut off. Write the file now in one complete write call, or split it into a second .go file."})
			continue
		}
		cutOffs = 0
		if round == maxRounds-1 {
			lastErr = fmt.Errorf("agent hit max tool rounds")
		}
	}

	status := "completed"
	if lastErr != nil {
		status = "failed"
	}
	// Drop busy before sink settle so credit-retry can Prompt the same project.
	release()
	if m.sink != nil {
		m.sink.RequestSettled(projectID, requestID, status)
	}
}

func callsBroken(calls []toolCall) bool {
	for _, tc := range calls {
		if toolCallBroken(tc) {
			return true
		}
	}
	return false
}

func toolCallBroken(tc toolCall) bool {
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		return true
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(args), &parsed) != nil {
		return true
	}
	switch tc.Function.Name {
	case "bash":
		cmd, _ := parsed["command"].(string)
		return strings.TrimSpace(cmd) == ""
	case "write":
		content, _ := parsed["content"].(string)
		return strings.TrimSpace(content) == ""
	}
	return false
}

func (m *Manager) awaitPermission(ctx context.Context, projectID, requestID int64, tool, detail string) bool {
	rpcID := fmt.Sprintf("perm-%d-%d-%d", projectID, requestID, time.Now().UnixNano())
	ch := make(chan bool, 1)
	m.mu.Lock()
	s := m.sessions[projectID]
	if s == nil {
		m.mu.Unlock()
		return false
	}
	s.pending[rpcID] = ch
	m.mu.Unlock()

	if m.sink != nil {
		m.sink.Permission(projectID, requestID, rpcID, tool, detail)
	}
	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		return false
	}
}

// Abort cancels the in-flight run for a project.
func (m *Manager) Abort(projectID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil || s.cancel == nil {
		return nil
	}
	s.cancel()
	return nil
}

// StopProject drops the session.
func (m *Manager) StopProject(projectID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[projectID]; s != nil && s.cancel != nil {
		s.cancel()
	}
	delete(m.sessions, projectID)
}

// SetModel updates the session model for the next prompt.
func (m *Manager) SetModel(projectID int64, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil {
		s = &session{pending: map[string]chan bool{}, question: map[string]chan string{}}
		m.sessions[projectID] = s
	}
	s.opts.Model = model
	return nil
}

// RespondValue answers a question (native agent rarely asks; kept for API parity).
func (m *Manager) RespondValue(projectID int64, rpcID, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil {
		return fmt.Errorf("no session")
	}
	ch := s.question[rpcID]
	if ch == nil {
		return fmt.Errorf("unknown question")
	}
	select {
	case ch <- value:
	default:
	}
	delete(s.question, rpcID)
	return nil
}

// RespondConfirm answers a permission prompt.
func (m *Manager) RespondConfirm(projectID int64, rpcID string, confirmed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil {
		return fmt.Errorf("no session")
	}
	ch := s.pending[rpcID]
	if ch == nil {
		return fmt.Errorf("unknown permission")
	}
	select {
	case ch <- confirmed:
	default:
	}
	delete(s.pending, rpcID)
	return nil
}

// RespondCancel rejects a pending question/permission.
func (m *Manager) RespondCancel(projectID int64, rpcID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil {
		return nil
	}
	if ch := s.pending[rpcID]; ch != nil {
		select {
		case ch <- false:
		default:
		}
		delete(s.pending, rpcID)
	}
	if ch := s.question[rpcID]; ch != nil {
		select {
		case ch <- "":
		default:
		}
		delete(s.question, rpcID)
	}
	return nil
}

// Streaming reports whether a run is in progress.
func (m *Manager) Streaming(projectID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	return s != nil && s.busy
}

// Stats returns cumulative token use for the project session.
func (m *Manager) Stats(projectID int64) *pi.SessionStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[projectID]
	if s == nil {
		return nil
	}
	cp := s.stats
	return &cp
}

// Shutdown cancels every session.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.cancel != nil {
			s.cancel()
		}
		delete(m.sessions, id)
	}
}

// ProbeModel checks the key, then asks the provider for one token.
// A refusal is returned so Make can stay on the desk instead of creating a project.
func (m *Manager) ProbeModel(ctx context.Context, full string) error {
	if err := m.HasKeyFor(full); err != nil {
		return err
	}
	_, _, err := m.client.complete(ctx, full, []chatMessage{{Role: "user", Content: "ping"}}, false, 1)
	if err != nil {
		return fmt.Errorf("did not answer: %s", err.Error())
	}
	return nil
}

// HasKeyFor reports whether credentials exist for a full model id.
func (m *Manager) HasKeyFor(full string) error {
	_, _, _, err := m.client.Keys.ResolveEndpoint(full)
	if err != nil {
		m.client.Keys = LoadKeys()
		_, _, _, err = m.client.Keys.ResolveEndpoint(full)
	}
	return err
}
