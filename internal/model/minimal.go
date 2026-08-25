package model

import "time"

const (
	PendingCommandStatusPending   = "pending"
	PendingCommandStatusClaimed   = "claimed"
	PendingCommandStatusCompleted = "completed"
	PendingCommandStatusFailed    = "failed"

	MinimalContinuationCreating = "creating"
	MinimalContinuationActive   = "active"
	MinimalContinuationFailed   = "failed"

	MinimalContinuationFailureDefinite  = "definite"
	MinimalContinuationFailureAmbiguous = "ambiguous"

	MinimalLinkedReady           = "ready"
	MinimalLinkedTelegramRunning = "telegram_running"
	MinimalLinkedReleasePending  = "release_pending"
	MinimalLinkedFailed          = "failed"
	MinimalLinkedTitlePending    = "pending"
	MinimalLinkedTitleSet        = "set"

	VoiceTargetNew    = "new"
	VoiceTargetThread = "thread"

	VoiceActionExecute = "voice_execute"
	VoiceActionCancel  = "voice_cancel"

	VoiceStatusPending   = "pending"
	VoiceStatusExecuted  = "executed"
	VoiceStatusCancelled = "cancelled"
	VoiceStatusExpired   = "expired"
	VoiceStatusFailed    = "failed"

	VoiceRouteStatusActive   = "active"
	VoiceRouteStatusConsumed = "consumed"
)

// Project is a fixed, locally configured workspace exposed to the minimal
// Telegram profile. CanonicalPath is populated with an absolute, evaluated
// directory path by projectregistry.Registry.
type Project struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	CanonicalPath string `json:"path"`
}

type InboundText struct {
	ChatID           int64
	TopicID          int64
	UserID           int64
	MessageID        int64
	ReplyToMessageID int64
	Text             string
	ReceivedAt       time.Time
}

type PendingCommand struct {
	ID             int64
	ThreadID       string
	SourceThreadID string
	SourceTurnID   string
	ProjectID      string
	ChatID         int64
	TopicID        int64
	Prompt         string
	Status         string
	CreatedAt      TimeString
}

type MinimalContinuationKey struct {
	ChatID         int64
	TopicID        int64
	SourceThreadID string
	SourceTurnID   string
}

type MinimalContinuation struct {
	Key          MinimalContinuationKey
	ProjectID    string
	ForkThreadID string
	Status       string
	FailureKind  string
	CreatedAt    TimeString
	UpdatedAt    TimeString
}

type MinimalLinkedThread struct {
	ChatKey            string
	ChatID             int64
	TopicID            int64
	ProjectID          string
	SourceThreadID     string
	LinkedThreadID     string
	SourceAnchorTurnID string
	SourceTitle        string
	DesiredTitle       string
	TitleState         string
	State              string
	ActiveTurnID       string
	WorkerGeneration   uint64
	LastBlockedAt      TimeString
	LastBlockedCode    string
	FailureKind        string
	CreatedAt          TimeString
	UpdatedAt          TimeString
	ReleasedAt         TimeString
}

type MinimalLinkedRelease struct {
	LinkedThreadID   string
	TurnID           string
	WorkerGeneration uint64
}

type MinimalPickerRoute struct {
	Token     string
	Action    string
	ProjectID string
	ThreadID  string
	Page      int
	ChatID    int64
	TopicID   int64
	Status    string
	ExpiresAt TimeString
}

type VoiceConfirmation struct {
	ID              int64
	ProjectID       string
	TargetKind      string
	ThreadID        string
	SourceTurnID    string
	Transcript      string
	SessionIdentity string
	Status          string
	ExpiresAt       TimeString
	ChatID          int64
	TopicID         int64
	MessageID       int64
	CreatedAt       TimeString
}

type VoiceCallbackRoute struct {
	Token     string
	VoiceID   int64
	Action    string
	Status    string
	ChatID    int64
	TopicID   int64
	MessageID int64
	CreatedAt TimeString
}

type VoiceClaim struct {
	Token           string
	Action          string
	SessionIdentity string
	ChatID          int64
	TopicID         int64
	MessageID       int64
	Now             time.Time
}
