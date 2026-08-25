package model

type NotifierObservation struct {
	ThreadID       string
	LastUpdatedAt  int64
	LastTurnID     string
	LastTurnStatus string
	BaselineReady  bool
	ReadRequired   bool
	DeferUntil     int64
	DiscoverySeq   int64
}
