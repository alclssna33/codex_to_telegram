package control

type EventKind string

const (
	EventTurnStarted        EventKind = "turn_started"
	EventTurnCompleted      EventKind = "turn_completed"
	EventThreadStatus       EventKind = "thread_status"
	EventToolStarted        EventKind = "tool_started"
	EventToolUpdated        EventKind = "tool_updated"
	EventToolCompleted      EventKind = "tool_completed"
	EventAgentMessage       EventKind = "agent_message"
	EventApprovalRequest    EventKind = "approval_request"
	EventInputRequest       EventKind = "input_request"
	EventLegacyTaskStarted  EventKind = "legacy_task_started"
	EventLegacyTaskComplete EventKind = "legacy_task_complete"
)

type NormalizedEvent struct {
	Kind        EventKind
	Method      string
	ThreadID    string
	ThreadTitle string
	ProjectName string
	TurnID      string
	TurnStatus  string
	ItemID      string
	ItemKind    string
	Label       string
	Status      string
	Output      string
	Text        string
	Phase       string
	RequestID   string
	Raw         Event
}
