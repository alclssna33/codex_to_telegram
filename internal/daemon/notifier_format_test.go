package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestRenderNotifierTerminalCompleted(t *testing.T) {
	snapshot := appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:    "thread-1",
			Title: "원본 서버용량",
			CWD:   `C:\work\37.개비공_홈페이지제작`,
		},
		LatestTurnID:     "turn-1",
		LatestTurnStatus: "completed",
		LatestFinalText:  "서버 용량을 점검했습니다.\n\n불필요한 백업 파일을 정리하면 됩니다.",
	}

	message := renderNotifierTerminal(snapshot)
	for _, want := range []string{"✅ 작업 완료", "폴더: 37.개비공_홈페이지제작", "대화: 원본 서버용량", "요약: 서버 용량을 점검했습니다."} {
		if !strings.Contains(message.Text, want) {
			t.Fatalf("message missing %q:\n%s", want, message.Text)
		}
	}
}

func TestNotifierSummaryIsUnicodeSafeAndBounded(t *testing.T) {
	got := notifierSummary(strings.Repeat("가", 350))
	if utf8.RuneCountInString(got) > 300 || !utf8.ValidString(got) {
		t.Fatalf("invalid summary length=%d valid=%t", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

func TestRenderNotifierTerminalDegradesMissingMetadata(t *testing.T) {
	message := renderNotifierTerminal(appserver.ThreadReadSnapshot{LatestTurnStatus: "completed"})
	if message.Text != "✅ 작업 완료\n\n작업이 완료되었습니다." {
		t.Fatalf("message = %q", message.Text)
	}
}

func TestNotifierSummarySkipsFencedCodeAndCleansMarkdown(t *testing.T) {
	text := "```go\nfmt.Println(\"opaque\")\n```\n> ## **요약:** `배포` _점검_ 완료\n- 두 번째   항목도\t확인\n\n이후 문단"
	got := notifierSummary(text)
	want := "요약: 배포 점검 완료 두 번째 항목도 확인"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestNotifierSummaryPrefersSentenceBoundaryWhenTruncating(t *testing.T) {
	text := strings.Repeat("가", 121) + "." + strings.Repeat("나", 250)
	got := notifierSummary(text)
	want := strings.Repeat("가", 121) + ".…"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestRenderNotifierTerminalFailureStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   string
	}{
		{
			name:   "failed",
			status: "failed",
			want:   "❌ 작업 실패\n\n폴더: bridge\n\n내용: 작업이 실패하거나 중단되었습니다.",
		},
		{
			name:   "interrupted",
			status: "interrupted",
			want:   "❌ 작업 실패\n\n폴더: bridge\n\n내용: 작업이 실패하거나 중단되었습니다.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := renderNotifierTerminal(appserver.ThreadReadSnapshot{
				Thread:           model.Thread{ID: "thread-1", Title: "thread-1", CWD: `/work/bridge`},
				LatestTurnStatus: tc.status,
				LatestFinalText:  "실패 상세는 노출하지 않습니다.",
			})
			if message.Text != tc.want {
				t.Fatalf("message = %q, want %q", message.Text, tc.want)
			}
		})
	}
}

func TestRenderNotifierTerminalOmitsOnlyMissingTitle(t *testing.T) {
	message := renderNotifierTerminal(appserver.ThreadReadSnapshot{
		Thread:           model.Thread{ID: "thread-1", CWD: `C:\work\37.개비공_홈페이지제작`},
		LatestTurnStatus: "completed",
		LatestFinalText:  "점검 완료",
	})
	want := "✅ 작업 완료\n\n폴더: 37.개비공_홈페이지제작\n\n요약: 점검 완료"
	if message.Text != want {
		t.Fatalf("message = %q, want %q", message.Text, want)
	}
}

func TestRenderNotifierTerminalOmitsOnlyMissingCWD(t *testing.T) {
	message := renderNotifierTerminal(appserver.ThreadReadSnapshot{
		Thread:           model.Thread{ID: "thread-1", Title: "원본 서버용량"},
		LatestTurnStatus: "completed",
		LatestFinalText:  "점검 완료",
	})
	want := "✅ 작업 완료\n\n대화: 원본 서버용량\n\n요약: 점검 완료"
	if message.Text != want {
		t.Fatalf("message = %q, want %q", message.Text, want)
	}
}
