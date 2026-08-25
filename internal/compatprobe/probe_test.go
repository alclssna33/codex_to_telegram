package compatprobe_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/compatprobe"
	"github.com/alclssna33/codex_to_telegram/internal/control"
)

func TestProbePassesOnlyWhenForeignApprovalIsActionable(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
		{Channel: "notification", Method: "turn/completed", Params: map[string]any{"thread": map[string]any{"id": "thr-1"}, "turn": map[string]any{"id": "turn-1", "status": "completed"}}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done"), completedThread("thr-1", "turn-1", "done")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{
		ForeignThreadID:  "thr-1",
		ApprovalDecision: "decline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ForeignApprovalSeen || !result.ForeignApprovalAnswered || !result.ForeignFinal || !result.ForeignTerminal || !result.RestartReconciled {
		t.Fatalf("incomplete gate: %+v", result)
	}
	if !result.Passed() {
		t.Fatalf("Passed() = false for complete gate: %+v", result)
	}
	if got, want := session.responses, []approvalResponse{{requestID: "req-1", decision: "decline"}}; !approvalResponsesEqual(got, want) {
		t.Fatalf("responses = %#v, want %#v", got, want)
	}
	if got, want := session.starts, 2; got != want {
		t.Fatalf("Start calls = %d, want %d", got, want)
	}
	if got, want := session.closes, 2; got != want {
		t.Fatalf("Close calls = %d, want %d", got, want)
	}
}

func TestProbeDoesNotAnswerApprovalWithoutRequestID(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done"), completedThread("thr-1", "turn-1", "done")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ForeignApprovalSeen {
		t.Fatal("ForeignApprovalSeen = false, want true for observed approval method")
	}
	if result.ForeignApprovalAnswered || result.Passed() {
		t.Fatalf("approval without request id passed gate: %+v", result)
	}
	if len(session.responses) != 0 {
		t.Fatalf("responses = %#v, want none", session.responses)
	}
}

func TestProbeIgnoresApprovalShapedNotification(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "notification", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForeignApprovalSeen || result.ForeignApprovalAnswered || len(session.responses) != 0 {
		t.Fatalf("notification counted as actionable approval: result=%+v responses=%#v", result, session.responses)
	}
}

func TestProbeIgnoresSimilarButNonExactApprovalMethod(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApprovalUnexpected", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForeignApprovalSeen || result.ForeignApprovalAnswered || len(session.responses) != 0 {
		t.Fatalf("similar non-exact method counted as approval: result=%+v responses=%#v", result, session.responses)
	}
}

func TestProbeIgnoresApprovalWithoutMatchingTurnID(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1"}},
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-2", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-other"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForeignApprovalSeen || result.ForeignApprovalAnswered || len(session.responses) != 0 {
		t.Fatalf("unmatched approval counted: result=%+v responses=%#v", result, session.responses)
	}
}

func TestProbeKeepsApprovalUnansweredWhenResponseFails(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done")}
	session.responseErr = errors.New("response rejected")

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ForeignApprovalSeen || result.ForeignApprovalAnswered || result.Passed() {
		t.Fatalf("failed response counted as answered: %+v", result)
	}
}

func TestProbeRequiresPostRestartTerminalFinalRead(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", "done"), activeThread("thr-1", "turn-1")}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RestartReconciled || result.Passed() {
		t.Fatalf("gate passed without post-restart terminal final: %+v", result)
	}
}

func TestProbeRequiresRestartReconciliationOfSameTurn(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{
		completedThread("thr-1", "turn-1", "done"),
		completedThread("thr-1", "turn-1", "done"),
		completedThread("thr-1", "turn-2", "other done"),
	}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RestartReconciled || result.Passed() {
		t.Fatalf("different turn satisfied restart reconciliation: %+v", result)
	}
}

func TestProbePollsThreadBeforeRestartWhenForeignEventsClose(t *testing.T) {
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{
		activeThread("thr-1", "turn-1"),
		completedThread("thr-1", "turn-1", "done"),
		completedThread("thr-1", "turn-1", "done"),
	}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed() {
		t.Fatalf("gate missed final polling state before restart: %+v", result)
	}
}

func TestProbeDoesNotCombineFinalEvidenceAcrossTurns(t *testing.T) {
	session := newFakeSession(nil)
	session.reads = []map[string]any{
		completedThread("thr-1", "turn-old", "old final"),
		activeThread("thr-1", "turn-new"),
		activeThread("thr-1", "turn-new"),
	}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForeignFinal || result.ForeignTerminal || result.Evidence.FinalLength != 0 || result.Evidence.FinalSHA256 != "" {
		t.Fatalf("gate combined old final with newer turn: %+v", result)
	}
	if got, want := result.Evidence.ForeignTurnID, "turn-new"; got != want {
		t.Fatalf("foreign turn id = %q, want %q", got, want)
	}
}

func TestProbeEvidenceContainsNoFinalBody(t *testing.T) {
	const final = "private final body must not appear"
	session := newFakeSession([]control.Event{
		{Channel: "server_request", Method: "item/commandExecution/requestApproval", ID: "req-1", Params: map[string]any{"threadId": "thr-1", "turnId": "turn-1"}},
	})
	session.reads = []map[string]any{completedThread("thr-1", "turn-1", final), completedThread("thr-1", "turn-1", final)}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), final) {
		t.Fatalf("probe evidence leaked final body: %s", data)
	}
	if result.Evidence.FinalLength != len(final) || len(result.Evidence.FinalSHA256) != 64 {
		t.Fatalf("final evidence = %+v, want length and SHA-256 only", result.Evidence)
	}
}

func TestProbeEvidenceRejectsMalformedIdentifierAndStatusObjects(t *testing.T) {
	const sentinel = "PRIVATE_SENTINEL_MUST_NOT_LEAK"
	malformed := map[string]any{
		"id": "thr-1",
		"turns": []any{map[string]any{
			"id":     map[string]any{"private": sentinel},
			"status": map[string]any{"type": map[string]any{"private": sentinel}},
			"items":  []any{},
		}},
	}
	session := newFakeSession(nil)
	session.reads = []map[string]any{malformed}

	result, err := compatprobe.New(session).Run(context.Background(), compatprobe.Options{ForeignThreadID: "thr-1"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinel) {
		t.Fatalf("malformed payload leaked into evidence: %s", data)
	}
	if result.Evidence.ForeignTurnID != "" || result.Evidence.ForeignStatus != "" {
		t.Fatalf("malformed identifier/status accepted: %+v", result.Evidence)
	}
}

type approvalResponse struct {
	requestID string
	decision  string
}

type fakeSession struct {
	events      chan control.Event
	reads       []map[string]any
	readIndex   int
	starts      int
	closes      int
	responses   []approvalResponse
	responseErr error
}

func newFakeSession(events []control.Event) *fakeSession {
	ch := make(chan control.Event, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return &fakeSession{events: ch}
}

func (s *fakeSession) Start(context.Context) error {
	s.starts++
	return nil
}

func (s *fakeSession) Close() error {
	s.closes++
	return nil
}

func (s *fakeSession) Subscribe() <-chan control.Event { return s.events }

func (s *fakeSession) ThreadList(context.Context, int, string) (map[string]any, error) {
	return map[string]any{"data": []any{map[string]any{"id": "thr-1"}}}, nil
}

func (s *fakeSession) ThreadRead(context.Context, string, bool) (map[string]any, error) {
	if len(s.reads) == 0 {
		return nil, errors.New("no thread reads configured")
	}
	index := s.readIndex
	if index >= len(s.reads) {
		index = len(s.reads) - 1
	}
	s.readIndex++
	return s.reads[index], nil
}

func (s *fakeSession) ThreadResume(context.Context, string, string) (map[string]any, error) {
	return nil, errors.New("unexpected ThreadResume")
}

func (s *fakeSession) ThreadStart(context.Context, string) (map[string]any, error) {
	return nil, errors.New("unexpected ThreadStart")
}

func (s *fakeSession) TurnStart(context.Context, string, string, string, control.TurnStartOptions) (map[string]any, error) {
	return nil, errors.New("unexpected TurnStart")
}

func (s *fakeSession) TurnSteer(context.Context, string, string, string) (map[string]any, error) {
	return nil, errors.New("unexpected TurnSteer")
}

func (s *fakeSession) TurnInterrupt(context.Context, string, string) error {
	return errors.New("unexpected TurnInterrupt")
}

func (s *fakeSession) RespondServerRequest(_ context.Context, requestID string, result map[string]any) error {
	decision, _ := result["decision"].(string)
	s.responses = append(s.responses, approvalResponse{requestID: requestID, decision: decision})
	return s.responseErr
}

func (s *fakeSession) ModelList(context.Context, bool) ([]control.ModelOption, error) {
	return nil, errors.New("unexpected ModelList")
}

func (s *fakeSession) CollaborationModeList(context.Context) ([]control.CollaborationModeOption, error) {
	return nil, errors.New("unexpected CollaborationModeList")
}

func (s *fakeSession) StderrTail() []string { return nil }

func completedThread(threadID, turnID, final string) map[string]any {
	return map[string]any{
		"id":     threadID,
		"status": "completed",
		"turns": []any{map[string]any{
			"id":     turnID,
			"status": "completed",
			"items": []any{map[string]any{
				"id":    "final-1",
				"type":  "agentMessage",
				"phase": "final_answer",
				"text":  final,
			}},
		}},
	}
}

func activeThread(threadID, turnID string) map[string]any {
	return map[string]any{
		"id":     threadID,
		"status": "inProgress",
		"turns": []any{map[string]any{
			"id":     turnID,
			"status": "inProgress",
			"items":  []any{},
		}},
	}
}

func approvalResponsesEqual(a, b []approvalResponse) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
