package control

import "testing"

func TestEventKindValues(t *testing.T) {
	tests := map[EventKind]string{
		EventTurnStarted:        "turn_started",
		EventTurnCompleted:      "turn_completed",
		EventThreadStatus:       "thread_status",
		EventToolStarted:        "tool_started",
		EventToolUpdated:        "tool_updated",
		EventToolCompleted:      "tool_completed",
		EventAgentMessage:       "agent_message",
		EventApprovalRequest:    "approval_request",
		EventInputRequest:       "input_request",
		EventLegacyTaskStarted:  "legacy_task_started",
		EventLegacyTaskComplete: "legacy_task_complete",
	}
	for got, want := range tests {
		if string(got) != want {
			t.Fatalf("event kind %q = %q, want %q", got, string(got), want)
		}
	}
}
