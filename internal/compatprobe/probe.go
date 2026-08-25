package compatprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/control"
)

const defaultObservationWindow = 2 * time.Minute

type Result struct {
	ForeignTerminal         bool     `json:"foreign_terminal"`
	ForeignFinal            bool     `json:"foreign_final"`
	ForeignApprovalSeen     bool     `json:"foreign_approval_seen"`
	ForeignApprovalAnswered bool     `json:"foreign_approval_answered"`
	RestartReconciled       bool     `json:"restart_reconciled"`
	Evidence                Evidence `json:"evidence"`
}

func (r Result) Passed() bool {
	return r.ForeignTerminal && r.ForeignFinal && r.ForeignApprovalSeen && r.ForeignApprovalAnswered && r.RestartReconciled
}

// Evidence deliberately contains only identifiers, statuses, timestamps,
// lengths, and hashes. Prompt, final, request, and local path bodies never
// leave the probe through this type.
type Evidence struct {
	ForeignThreadID   string `json:"foreign_thread_id,omitempty"`
	ForeignTurnID     string `json:"foreign_turn_id,omitempty"`
	ApprovalRequestID string `json:"approval_request_id,omitempty"`
	ForeignStatus     string `json:"foreign_status,omitempty"`
	ObservedAt        string `json:"observed_at"`
	FinalLength       int    `json:"final_length,omitempty"`
	FinalSHA256       string `json:"final_sha256,omitempty"`
}

type Options struct {
	ForeignThreadID   string
	ApprovalDecision  string
	ObservationWindow time.Duration
}

type Probe struct {
	session control.RuntimeSession
	now     func() time.Time
}

func New(session control.RuntimeSession) *Probe {
	return &Probe{session: session, now: time.Now}
}

func (p *Probe) Run(ctx context.Context, options Options) (result Result, err error) {
	if p == nil || p.session == nil {
		return Result{}, errors.New("compatibility probe requires a runtime session")
	}
	decision, err := approvalDecision(options.ApprovalDecision)
	if err != nil {
		return Result{}, err
	}
	events := p.session.Subscribe()
	if err := p.session.Start(ctx); err != nil {
		return Result{}, fmt.Errorf("start app server session: %w", err)
	}
	defer func() {
		if closeErr := p.session.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close app server session: %w", closeErr)
		}
	}()

	threadID, err := p.discoverThread(ctx, strings.TrimSpace(options.ForeignThreadID))
	if err != nil {
		return Result{}, err
	}
	result.Evidence.ForeignThreadID = threadID
	result.Evidence.ObservedAt = p.now().UTC().Format(time.RFC3339Nano)
	if err := p.readForeignThread(ctx, threadID, &result); err != nil {
		return Result{}, err
	}

	window := options.ObservationWindow
	if window <= 0 {
		window = defaultObservationWindow
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	observe := true
	for observe && !(result.ForeignTerminal && result.ForeignFinal && result.ForeignApprovalAnswered) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-timer.C:
			observe = false
		case event, ok := <-events:
			if !ok {
				observe = false
				continue
			}
			if !eventMatchesThread(event, threadID, result.Evidence.ForeignTurnID) {
				continue
			}
			if isApprovalMethod(event.Method) {
				if !isApprovalRequestForTurn(event, threadID, result.Evidence.ForeignTurnID) {
					continue
				}
				result.ForeignApprovalSeen = true
				requestID := requestIDString(event.ID)
				if requestID != "" {
					result.Evidence.ApprovalRequestID = requestID
					if err := p.session.RespondServerRequest(ctx, requestID, map[string]any{"decision": decision}); err == nil {
						result.ForeignApprovalAnswered = true
					}
				}
			} else {
				result.observeTurn(nestedID(event.Params, "turnId", "turn"))
			}
			if isTerminalEvent(event) || isFinalAgentEvent(event) {
				if err := p.readForeignThread(ctx, threadID, &result); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if err := p.readForeignThread(ctx, threadID, &result); err != nil {
		return Result{}, err
	}

	if err := p.session.Close(); err != nil {
		return Result{}, fmt.Errorf("close before reconciliation: %w", err)
	}
	if err := p.session.Start(ctx); err != nil {
		return Result{}, fmt.Errorf("restart for reconciliation: %w", err)
	}
	reconciledThreadID, err := p.discoverThread(ctx, threadID)
	if err != nil {
		return result, nil
	}
	reconciled := Result{}
	if err := p.readForeignThread(ctx, reconciledThreadID, &reconciled); err != nil {
		return result, nil
	}
	result.RestartReconciled = result.Evidence.ForeignTurnID != "" &&
		reconciled.Evidence.ForeignTurnID == result.Evidence.ForeignTurnID &&
		reconciled.ForeignTerminal && reconciled.ForeignFinal
	return result, nil
}

func (p *Probe) discoverThread(ctx context.Context, requestedID string) (string, error) {
	list, err := p.session.ThreadList(ctx, 100, "")
	if err != nil {
		return "", fmt.Errorf("list foreign threads: %w", err)
	}
	ids := threadIDs(list)
	if requestedID == "" {
		if len(ids) == 0 {
			return "", errors.New("no foreign thread discovered")
		}
		return ids[0], nil
	}
	for _, id := range ids {
		if id == requestedID {
			return requestedID, nil
		}
	}
	return "", errors.New("requested foreign thread was not discovered")
}

func (p *Probe) readForeignThread(ctx context.Context, threadID string, result *Result) error {
	payload, err := p.session.ThreadRead(ctx, threadID, true)
	if err != nil {
		return fmt.Errorf("read foreign thread: %w", err)
	}
	facts := snapshotFacts(payload)
	if facts.threadID != "" && facts.threadID != threadID {
		return errors.New("thread read returned a different thread id")
	}
	if facts.turnID != "" {
		result.observeTurn(facts.turnID)
	}
	if facts.status != "" {
		result.Evidence.ForeignStatus = facts.status
	}
	if facts.terminal {
		result.ForeignTerminal = true
	}
	if facts.final != "" {
		result.ForeignFinal = true
		result.Evidence.FinalLength = len(facts.final)
		sum := sha256.Sum256([]byte(facts.final))
		result.Evidence.FinalSHA256 = hex.EncodeToString(sum[:])
	}
	return nil
}

func (r *Result) observeTurn(turnID string) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	if r.Evidence.ForeignTurnID != "" && r.Evidence.ForeignTurnID != turnID {
		r.ForeignTerminal = false
		r.ForeignFinal = false
		r.ForeignApprovalSeen = false
		r.ForeignApprovalAnswered = false
		r.RestartReconciled = false
		r.Evidence.ApprovalRequestID = ""
		r.Evidence.ForeignStatus = ""
		r.Evidence.FinalLength = 0
		r.Evidence.FinalSHA256 = ""
	}
	r.Evidence.ForeignTurnID = turnID
}

type threadFacts struct {
	threadID string
	turnID   string
	status   string
	terminal bool
	final    string
}

func snapshotFacts(result map[string]any) threadFacts {
	payload := result
	if nested, ok := result["thread"].(map[string]any); ok && nested != nil {
		payload = nested
	}
	facts := threadFacts{threadID: identifierFact(payload["id"])}
	if facts.threadID == "" {
		facts.threadID = identifierFact(payload["threadId"])
	}
	turns, _ := payload["turns"].([]any)
	if len(turns) == 0 {
		facts.status = statusFact(payload["status"])
		facts.terminal = terminalStatus(facts.status)
		return facts
	}
	turn, _ := turns[len(turns)-1].(map[string]any)
	facts.turnID = identifierFact(turn["id"])
	facts.status = statusFact(turn["status"])
	facts.terminal = terminalStatus(facts.status)
	items, _ := turn["items"].([]any)
	for i := len(items) - 1; i >= 0; i-- {
		item, _ := items[i].(map[string]any)
		if !strings.EqualFold(textFact(item["type"]), "agentMessage") {
			continue
		}
		phase := strings.ToLower(textFact(item["phase"]))
		if phase != "" && phase != "final_answer" && phase != "finalanswer" {
			continue
		}
		text := agentMessageText(item)
		if text != "" && (phase != "" || facts.terminal) {
			facts.final = text
			break
		}
	}
	return facts
}

func agentMessageText(item map[string]any) string {
	if text := textFact(item["text"]); text != "" {
		return text
	}
	content, _ := item["content"].([]any)
	parts := make([]string, 0, len(content))
	for _, value := range content {
		part, _ := value.(map[string]any)
		if text := textFact(part["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func threadIDs(result map[string]any) []string {
	for _, key := range []string{"data", "threads", "items"} {
		items, ok := result[key].([]any)
		if !ok {
			continue
		}
		ids := make([]string, 0, len(items))
		for _, value := range items {
			item, _ := value.(map[string]any)
			if nested, ok := item["thread"].(map[string]any); ok && nested != nil {
				item = nested
			}
			id := identifierFact(item["id"])
			if id == "" {
				id = identifierFact(item["threadId"])
			}
			if id != "" {
				ids = append(ids, id)
			}
		}
		return ids
	}
	return nil
}

func eventMatchesThread(event control.Event, threadID, turnID string) bool {
	eventThreadID := nestedID(event.Params, "threadId", "thread")
	if eventThreadID != "" {
		return eventThreadID == threadID
	}
	eventTurnID := nestedID(event.Params, "turnId", "turn")
	return eventTurnID != "" && turnID != "" && eventTurnID == turnID
}

func nestedID(params map[string]any, directKey, nestedKey string) string {
	if id := identifierFact(params[directKey]); id != "" {
		return id
	}
	if nested, ok := params[nestedKey].(map[string]any); ok {
		return identifierFact(nested["id"])
	}
	return ""
}

func isApprovalMethod(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "commandExecution/requestApproval":
		return true
	default:
		return false
	}
}

func isApprovalRequestForTurn(event control.Event, threadID, turnID string) bool {
	if event.Channel != "server_request" || !isApprovalMethod(event.Method) {
		return false
	}
	eventThreadID := nestedID(event.Params, "threadId", "thread")
	eventTurnID := nestedID(event.Params, "turnId", "turn")
	return eventThreadID != "" && eventThreadID == threadID &&
		eventTurnID != "" && eventTurnID == turnID
}

func isTerminalEvent(event control.Event) bool {
	if !strings.EqualFold(strings.TrimSpace(event.Method), "turn/completed") {
		return false
	}
	status := statusFact(event.Params["status"])
	if turn, ok := event.Params["turn"].(map[string]any); ok {
		status = statusFact(turn["status"])
	}
	return status == "" || terminalStatus(status)
}

func isFinalAgentEvent(event control.Event) bool {
	method := strings.ToLower(strings.TrimSpace(event.Method))
	if method != "item/completed" && method != "item/updated" {
		return false
	}
	item, _ := event.Params["item"].(map[string]any)
	return strings.EqualFold(textFact(item["type"]), "agentMessage") && strings.EqualFold(textFact(item["phase"]), "final_answer")
}

func terminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "interrupted", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func statusFact(value any) string {
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"type", "status"} {
			if out := canonicalStatus(textFact(object[key])); out != "" {
				return out
			}
		}
	}
	return canonicalStatus(textFact(value))
}

func canonicalStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "interrupted":
		return "interrupted"
	case "cancelled":
		return "cancelled"
	case "canceled":
		return "canceled"
	case "inprogress":
		return "inProgress"
	case "running":
		return "running"
	case "active":
		return "active"
	case "idle":
		return "idle"
	case "notstarted":
		return "notStarted"
	case "waitingonapproval":
		return "waitingOnApproval"
	case "waitingonuserinput":
		return "waitingOnUserInput"
	case "waitingoninput":
		return "waitingOnInput"
	default:
		return ""
	}
}

func identifierFact(value any) string {
	return textFact(value)
}

func textFact(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "<nil>" {
		return ""
	}
	return text
}

func requestIDString(value any) string {
	switch typed := value.(type) {
	case string:
		return textFact(typed)
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return typed.String()
		}
		return ""
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint8:
		return strconv.FormatUint(uint64(typed), 10)
	case uint16:
		return strconv.FormatUint(uint64(typed), 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return ""
	}
}

func approvalDecision(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", "decline":
		return "decline", nil
	case "accept":
		return "accept", nil
	case "acceptForSession":
		return "acceptForSession", nil
	default:
		return "", errors.New("approval decision must be accept, acceptForSession, or decline")
	}
}
