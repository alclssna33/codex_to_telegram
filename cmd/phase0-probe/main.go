package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/compatprobe"
)

func main() {
	os.Exit(run())
}

func run() int {
	threadID := flag.String("thread-id", "", "foreign Desktop/CLI thread ID; defaults to the latest listed thread")
	decision := flag.String("decision", "decline", "approval decision: accept, acceptForSession, or decline")
	observationWindow := flag.Duration("observe", 2*time.Minute, "maximum time to observe an actionable approval request")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "App Server request timeout")
	codexBin := flag.String("codex-bin", "codex", "Codex executable")
	listenURL := flag.String("listen", "stdio://", "Codex App Server listen transport")
	cwd := flag.String("cwd", ".", "working directory used to start Codex App Server")
	flag.Parse()

	client := appserver.NewClient(*codexBin, *listenURL, *cwd, *requestTimeout)
	result, err := compatprobe.New(client).Run(context.Background(), compatprobe.Options{
		ForeignThreadID:   *threadID,
		ApprovalDecision:  *decision,
		ObservationWindow: *observationWindow,
	})
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err != nil {
		message := err.Error()
		sum := sha256.Sum256([]byte(message))
		_ = encoder.Encode(struct {
			Status      string `json:"status"`
			ObservedAt  string `json:"observed_at"`
			ErrorLength int    `json:"error_length"`
			ErrorSHA256 string `json:"error_sha256"`
		}{
			Status:      "error",
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			ErrorLength: len(message),
			ErrorSHA256: hex.EncodeToString(sum[:]),
		})
		return 1
	}
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		return 1
	}
	if !result.Passed() {
		return 1
	}
	return 0
}
