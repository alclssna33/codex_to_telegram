package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const minimalSelectionSummarySentinel = "SELECT_SUMMARY_SENTINEL_733e"

func TestMinimalExistingThreadPageShowsEightRowsWithOpaqueNavigation(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	fake := &minimalCatalogSession{pages: map[string]map[string]any{
		"": threadListPayloadWithCursor("cursor-2",
			threadListItem("thread-1", "Thread 1", project.CanonicalPath, 901, "completed", ""),
			threadListItem("thread-2", "Thread 2", project.CanonicalPath, 902, "completed", ""),
			threadListItem("thread-3", "Thread 3", project.CanonicalPath, 903, "completed", ""),
			threadListItem("thread-4", "Thread 4", project.CanonicalPath, 904, "completed", ""),
			threadListItem("thread-5", "Thread 5", project.CanonicalPath, 905, "completed", ""),
			threadListItem("thread-6", "Thread 6", project.CanonicalPath, 906, "completed", ""),
			threadListItem("thread-7", "Thread 7", project.CanonicalPath, 907, "completed", ""),
			threadListItem("thread-8", "Thread 8", project.CanonicalPath, 908, "completed", ""),
		),
		"cursor-2": threadListPayload(
			threadListItem("thread-9", "Thread 9", project.CanonicalPath, 909, "completed", ""),
			threadListItem("other", "Other", svc.cfg.Projects[1].CanonicalPath, 999, "completed", ""),
		),
	}}
	useCatalogSession(svc, fake)

	first, err := svc.minimalExistingThreadPage(ctx, 7, 0, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rowButtonLabels(first.Buttons[:8]), []string{"Thread 9", "Thread 8", "Thread 7", "Thread 6", "Thread 5", "Thread 4", "Thread 3", "Thread 2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page 1 rows = %v, want %v", got, want)
	}
	if got, want := rowButtonLabels(first.Buttons[8:]), []string{"다음", "뒤로"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page 1 nav = %v, want %v", got, want)
	}

	second, err := svc.minimalExistingThreadPage(ctx, 7, 0, project.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rowButtonLabels(second.Buttons[:1]), []string{"Thread 1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page 2 rows = %v, want %v", got, want)
	}
	if got, want := rowButtonLabels(second.Buttons[1:]), []string{"이전", "뒤로"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page 2 nav = %v, want %v", got, want)
	}

	for _, response := range []*DirectResponse{first, second} {
		for _, row := range response.Buttons {
			for _, button := range row {
				for _, forbidden := range []string{"thread-", "Thread ", project.CanonicalPath, filepath.Base(project.CanonicalPath)} {
					if strings.Contains(button.CallbackData, forbidden) {
						t.Fatalf("callback data for %q leaks %q: %q", button.Text, forbidden, button.CallbackData)
					}
				}
			}
		}
	}
	for _, call := range fake.listCalls {
		if call.limit > 100 {
			t.Fatalf("ThreadList limit = %d, want <= 100", call.limit)
		}
	}
}

func TestMinimalProjectActionsClearsBindingFromAnotherProject(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "second-thread", CWD: svc.cfg.Projects[1].CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "second-thread", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}

	response, err := svc.minimalProjectActions(ctx, 7, 0, svc.cfg.Projects[0])
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "작업폴더: Bridge") || len(response.Buttons) != 1 {
		t.Fatalf("project actions response = %#v", response)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("binding after project switch = %#v, want cleared", binding)
	}
}

func TestMinimalBoundThreadForProjectFreshReadsExactCWDAndPersistsScrubbedMetadata(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "bound-thread", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"bound-thread": threadReadPayload("bound-thread", "Useful title", project.CanonicalPath, "turn-1", "completed", minimalSelectionSummarySentinel),
	}}}
	useCatalogSession(svc, fake)

	thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread == nil || thread.ID != "bound-thread" || thread.CWD != project.CanonicalPath || thread.ProjectName != project.DisplayName {
		t.Fatalf("bound thread = %#v", thread)
	}
	stored, err := svc.store.GetThread(ctx, "bound-thread")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Title != "bound-thread" || stored.LastPreview != "" || bytes.Contains(stored.Raw, []byte(minimalSelectionSummarySentinel)) {
		t.Fatalf("stored thread = %#v, want scrubbed metadata without summary", stored)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, minimalSelectionSummarySentinel); err != nil {
		t.Fatal(err)
	}
}

func TestMinimalBoundThreadForProjectInvalidBindings(t *testing.T) {
	cases := []struct {
		name            string
		bind            bool
		payload         map[string]any
		readErr         error
		wantBindingKept bool
	}{
		{name: "absent binding"},
		{name: "different project", bind: true, payload: threadReadPayload("bound-thread", "Moved", "", "turn-1", "completed", "")},
		{name: "fresh read failure preserves binding", bind: true, readErr: errors.New("temporary read failure"), wantBindingKept: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			ctx := context.Background()
			project := svc.cfg.Projects[0]
			if tc.name == "different project" {
				tc.payload = threadReadPayload("bound-thread", "Moved", svc.cfg.Projects[1].CanonicalPath, "turn-1", "completed", "")
			}
			if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
				t.Fatal(err)
			}
			if tc.bind {
				if err := svc.store.SetBinding(ctx, 7, 0, "bound-thread", model.BindingModeBound); err != nil {
					t.Fatal(err)
				}
			}
			fake := &minimalCatalogSession{stubSession: stubSession{
				threadReads:   map[string]map[string]any{"bound-thread": tc.payload},
				threadReadErr: tc.readErr,
			}}
			useCatalogSession(svc, fake)

			thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
			if tc.readErr != nil {
				if err == nil {
					t.Fatal("fresh read failure returned nil error")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if thread != nil {
				t.Fatalf("bound thread = %#v, want nil", thread)
			}
			binding, err := svc.store.GetBinding(ctx, 7, 0)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantBindingKept {
				if binding == nil || binding.ThreadID != "bound-thread" {
					t.Fatalf("binding after %s = %#v, want preserved", tc.name, binding)
				}
			} else if binding != nil {
				t.Fatalf("binding after %s = %#v, want nil", tc.name, binding)
			}
		})
	}
}

func TestMinimalBoundThreadForProjectRejectsChildCWDAndClearsBinding(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	child := filepath.Join(project.CanonicalPath, "child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "bound-thread", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"bound-thread": threadReadPayload("bound-thread", "Child", child, "turn-1", "completed", ""),
	}}}
	useCatalogSession(svc, fake)

	thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread != nil {
		t.Fatalf("bound thread = %#v, want nil", thread)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("child cwd binding = %#v, want cleared", binding)
	}
}

func TestSelectMinimalExistingThreadFreshReadsBindsAndSummarizes(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	payload := threadReadPayload("thread-1", "Useful title", project.CanonicalPath, "turn-1", "completed", minimalSelectionSummarySentinel)
	payload["thread"].(map[string]any)["source"] = "vscode"
	payload["thread"].(map[string]any)["originator"] = "Codex Desktop"
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"thread-1": payload,
	}}}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "thread-1", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "최근 요약") || !strings.Contains(response.Text, minimalSelectionSummarySentinel) {
		t.Fatalf("response = %#v, want one-time recent summary", response)
	}
	if response.ThreadID != "thread-1" || response.TurnID != "turn-1" {
		t.Fatalf("response route = %#v, want thread-1/turn-1", response)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "thread-1" {
		t.Fatalf("binding = %#v, want thread-1", binding)
	}
	if got := fake.ResumeCalls("thread-1"); got != 0 {
		t.Fatalf("resume calls for thread-1 = %d, want 0", got)
	}
	pcOrigin, err := svc.store.GetState(ctx, minimalPCOriginThreadStatePrefix+"thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if pcOrigin != "1" {
		t.Fatalf("PC-origin marker = %q, want durable marker", pcOrigin)
	}
	stored, err := svc.store.GetThread(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Title != "thread-1" || stored.LastPreview != "" || bytes.Contains(stored.Raw, []byte(minimalSelectionSummarySentinel)) {
		t.Fatalf("stored thread = %#v, want scrubbed metadata without summary", stored)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, minimalSelectionSummarySentinel); err != nil {
		t.Fatal(err)
	}
}

func TestMinimalPickerSelectsExactSourceDespiteHistoricalLinkedThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-123456789")
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"source-1":         threadReadPayload("source-1", "Source title", project.CanonicalPath, "source-turn-1", "completed", "source summary"),
		"linked-123456789": threadReadPayload("linked-123456789", "안클코 · 텔레그램 연동", project.CanonicalPath, "linked-turn-2", "completed", minimalSelectionSummarySentinel),
	}}}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "source-1", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "source-1" || response.TurnID != "source-turn-1" {
		t.Fatalf("response=%#v, want exact source selection", response)
	}
	if !strings.Contains(response.Text, "Source title") || !strings.Contains(response.Text, "source summary") {
		t.Fatalf("response text=%q, want source title and source summary", response.Text)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "source-1" {
		t.Fatalf("binding=%#v, want exact source binding", binding)
	}
	if got, want := fake.threadReadCalls, []minimalThreadReadCall{{threadID: "source-1", includeTurns: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread read calls=%#v, want %#v", got, want)
	}
}

func TestMinimalPickerHistoricalLinkedReadFailureStillBindsExactSource(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-123456789")
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"source-1": threadReadPayload("source-1", "Source title", project.CanonicalPath, "source-turn-1", "completed", "source summary"),
	}}}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "source-1", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "source-1" || !strings.Contains(response.Text, "source summary") {
		t.Fatalf("response=%#v, want source selection without reading linked child", response)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "source-1" {
		t.Fatalf("binding=%#v, want exact source binding", binding)
	}
}

func TestMinimalBoundThreadForProjectReturnsExactSourceDespiteHistoricalLinkedThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-123456789")
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "source-1", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"source-1":         threadReadPayload("source-1", "Source title", project.CanonicalPath, "source-turn-1", "completed", "source summary"),
		"linked-123456789": threadReadPayload("linked-123456789", "Linked title", project.CanonicalPath, "linked-turn-2", "completed", "linked summary"),
	}}}
	useCatalogSession(svc, fake)

	thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread == nil || thread.ID != "source-1" {
		t.Fatalf("thread=%#v, want exact source thread", thread)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "source-1" {
		t.Fatalf("binding=%#v, want exact source binding", binding)
	}
}

func TestMinimalBoundThreadForProjectExplicitLinkedBindingReturnsLinkedThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-123456789")
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "linked-123456789", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"linked-123456789": threadReadPayload("linked-123456789", "Linked title", project.CanonicalPath, "linked-turn-2", "completed", "linked summary"),
	}}}
	useCatalogSession(svc, fake)

	thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread == nil || thread.ID != "linked-123456789" {
		t.Fatalf("thread=%#v, want explicitly bound linked thread", thread)
	}
	if got, want := fake.threadReadCalls, []minimalThreadReadCall{{threadID: "linked-123456789", includeTurns: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread read calls=%#v, want %#v", got, want)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "linked-123456789" {
		t.Fatalf("binding=%#v, want explicit linked binding", binding)
	}
}

func TestPlainExplicitLinkedBindingReadFailureDoesNotStartNewThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-123456789")
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "linked-123456789", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{}}}
	useCatalogSession(svc, fake)
	workers := installWorkerFactory(svc)

	response, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "plain after missing explicit linked", ReceivedAt: svc.now()})
	if err == nil {
		t.Fatalf("response=%#v err=nil, want fail-closed error", response)
	}
	if got := len(workers.Sessions()); got != 0 {
		t.Fatalf("worker sessions=%d, want no new thread start", got)
	}
	binding, bindErr := svc.store.GetBinding(ctx, 7, 0)
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	if binding == nil || binding.ThreadID != "linked-123456789" {
		t.Fatalf("binding=%#v, want explicit linked binding preserved", binding)
	}
}

func TestMinimalBoundThreadForProjectRejectsLinkedBindingFromDifferentChat(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	seedReadyLinkedThread(t, svc, 8, 0, "source-other", "source-turn-1", "linked-123456789")
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "linked-123456789", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"linked-123456789": threadReadPayload("linked-123456789", "Linked title", project.CanonicalPath, "linked-turn-2", "completed", "linked summary"),
	}}}
	useCatalogSession(svc, fake)

	thread, err := svc.minimalBoundThreadForProject(ctx, 7, 0, project.ID)
	if err == nil {
		t.Fatalf("thread=%#v err=nil, want linked binding context mismatch error", thread)
	}
	if len(fake.threadReadCalls) != 0 {
		t.Fatalf("thread read calls=%#v, want no linked read for wrong chat", fake.threadReadCalls)
	}
	binding, bindErr := svc.store.GetBinding(ctx, 7, 0)
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	if binding == nil || binding.ThreadID != "linked-123456789" {
		t.Fatalf("binding=%#v, want preserved linked binding", binding)
	}
}

func TestSelectMinimalExistingThreadFallsBackToSnapshotWhenTurnsReadRejectsPaginatedHistory(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	fake := &minimalCatalogSession{
		stubSession: stubSession{threadReads: map[string]map[string]any{
			"paginated-thread": threadReadSnapshotPayload("paginated-thread", "Paginated history", project.CanonicalPath, "completed"),
		}},
		threadReadErrByIncludeTurns: map[bool]error{
			true: errors.New("json-rpc error: paginated_threads is not supported yet"),
		},
	}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "paginated-thread", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "paginated-thread" {
		t.Fatalf("response = %#v, want selected paginated-thread", response)
	}
	if response.TurnID != "" {
		t.Fatalf("response turn id = %q, want empty snapshot-only turn id", response.TurnID)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.ThreadID != "paginated-thread" {
		t.Fatalf("binding = %#v, want paginated-thread", binding)
	}
	if got := fake.ResumeCalls("paginated-thread"); got != 0 {
		t.Fatalf("resume calls for paginated-thread = %d, want 0", got)
	}
	if got, want := fake.threadReadCalls, []minimalThreadReadCall{
		{threadID: "paginated-thread", includeTurns: true},
		{threadID: "paginated-thread", includeTurns: false},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread read calls = %#v, want %#v", got, want)
	}
}

func TestSelectMinimalExistingThreadDoesNotFallbackForUnrelatedTurnsReadError(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	fake := &minimalCatalogSession{
		stubSession: stubSession{threadReads: map[string]map[string]any{
			"thread-1": threadReadSnapshotPayload("thread-1", "Useful title", project.CanonicalPath, "completed"),
		}},
		threadReadErrByIncludeTurns: map[bool]error{
			true: errors.New("temporary read failure"),
		},
	}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "thread-1", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || strings.Contains(response.Text, "Useful title") {
		t.Fatalf("response = %#v, want safe retry without snapshot fallback", response)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("binding = %#v, want nil", binding)
	}
	if got := fake.ResumeCalls("thread-1"); got != 0 {
		t.Fatalf("resume calls for thread-1 = %d, want 0", got)
	}
	if got, want := fake.threadReadCalls, []minimalThreadReadCall{{threadID: "thread-1", includeTurns: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread read calls = %#v, want %#v", got, want)
	}
}

func TestSelectMinimalExistingThreadFailuresLeaveNoNewBindingOrTurn(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{name: "deleted thread"},
		{name: "changed cwd", payload: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			ctx := context.Background()
			project := svc.cfg.Projects[0]
			payload := tc.payload
			if tc.name == "changed cwd" {
				payload = threadReadPayload("thread-1", "Moved", svc.cfg.Projects[1].CanonicalPath, "turn-1", "completed", minimalSelectionSummarySentinel)
			}
			fake := &minimalCatalogSession{
				stubSession: stubSession{
					threadReads: map[string]map[string]any{"thread-1": payload},
				},
			}
			useCatalogSession(svc, fake)

			response, err := selectExisting(t, svc, "thread-1", project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || strings.Contains(response.Text, minimalSelectionSummarySentinel) {
				t.Fatalf("failure response = %#v, want safe retry without summary", response)
			}
			binding, err := svc.store.GetBinding(ctx, 7, 0)
			if err != nil {
				t.Fatal(err)
			}
			if binding != nil {
				t.Fatalf("failure stored binding = %#v, want nil", binding)
			}
			if len(fake.turnStartCalls) != 0 || len(fake.turnSteerCalls) != 0 {
				t.Fatalf("selection started or steered a turn: starts=%#v steers=%#v", fake.turnStartCalls, fake.turnSteerCalls)
			}
			if err := assertSQLiteFilesDoNotContain(t, svc, minimalSelectionSummarySentinel); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMinimalExistingThreadStaleSelectionRefreshesCurrentPickerPage(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	fake := &minimalCatalogSession{pages: map[string]map[string]any{
		"": threadListPayload(
			threadListItem("gone-thread", "Gone thread", project.CanonicalPath, 902, "completed", ""),
			threadListItem("current-thread", "Current thread", project.CanonicalPath, 901, "completed", ""),
		),
	}}
	useCatalogSession(svc, fake)
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}

	page, err := svc.minimalExistingThreadPage(ctx, 7, 0, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	staleToken := minimalThreadPickerCallbackTokenForButton(t, page.Buttons, "Gone thread")
	fake.pages[""] = threadListPayload(
		threadListItem("current-thread", "Current thread", project.CanonicalPath, 901, "completed", ""),
	)
	sender := &recordingSender{}
	svc.SetSender(sender)

	response, err := svc.HandleCallback(ctx, 7, 0, 55, 7, staleToken)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Text != "" || response.CallbackText == "" {
		t.Fatalf("callback response = %#v, want callback-only acknowledgement", response)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits = %#v, want refreshed picker edit", sender.edits)
	}
	edit := sender.edits[0]
	if edit.chatID != 7 || edit.topicID != 0 || edit.messageID != 55 {
		t.Fatalf("edit route = %#v", edit)
	}
	if !strings.Contains(edit.text, "이 대화를 열 수 없습니다.") || !strings.Contains(edit.text, "기존 대화를 선택하세요.") {
		t.Fatalf("edit text = %q, want retry guidance and refreshed picker", edit.text)
	}
	if strings.Contains(edit.text, "Gone thread") {
		t.Fatalf("edit text = %q, want stale row removed", edit.text)
	}
	if got, want := rowButtonLabels(edit.buttons), []string{"Current thread", "뒤로"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refreshed buttons = %v, want %v", got, want)
	}
	if binding, err := svc.store.GetBinding(ctx, 7, 0); err != nil {
		t.Fatal(err)
	} else if binding != nil {
		t.Fatalf("stale selection stored binding = %#v, want nil", binding)
	}
	if got := fake.ResumeCalls("gone-thread"); got != 0 {
		t.Fatalf("resume calls for stale thread = %d, want 0", got)
	}
}

func TestMinimalExistingThreadCallbackRejectsStaleProjectAfterStartSwitch(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem("a-thread", "A thread", firstProject.CanonicalPath, 901, "completed", "")),
		},
		stubSession: stubSession{threadReads: map[string]map[string]any{
			"a-thread": threadReadPayload("a-thread", "A thread", firstProject.CanonicalPath, "turn-a", "completed", minimalSelectionSummarySentinel),
		}},
	}
	useCatalogSession(svc, fake)

	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	firstProjectToken := picker.Buttons[0][0].CallbackData
	actions, err := svc.HandleCallback(ctx, 7, 0, 51, 7, firstProjectToken)
	if err != nil {
		t.Fatal(err)
	}
	openToken := minimalThreadPickerCallbackTokenForButton(t, actions.Buttons, "기존 대화 열기")
	page, err := svc.HandleCallback(ctx, 7, 0, 52, 7, openToken)
	if err != nil {
		t.Fatal(err)
	}
	staleAThreadToken := minimalThreadPickerCallbackTokenForButton(t, page.Buttons, "A thread")

	switchPicker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	secondProjectToken := switchPicker.Buttons[1][0].CallbackData
	if _, err := svc.HandleCallback(ctx, 7, 0, 53, 7, secondProjectToken); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleCallback(ctx, 7, 0, 52, 7, staleAThreadToken)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "/start") {
		t.Fatalf("stale A callback response = %#v, want fail-closed stale guidance", response)
	}
	selected, err := svc.store.GetSelectedProject(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if selected != secondProject.ID {
		t.Fatalf("selected project after stale A callback = %q, want %q", selected, secondProject.ID)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("binding after stale A callback = %#v, want unchanged nil B binding", binding)
	}
	if got := fake.ResumeCalls("a-thread"); got != 0 {
		t.Fatalf("resume calls for stale A thread = %d, want 0", got)
	}
}

func TestMinimalExistingThreadCallbackRechecksProjectAfterPostConsumeInterleaving(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem("a-race-thread", "A race thread", firstProject.CanonicalPath, 901, "completed", "")),
		},
		stubSession: stubSession{threadReads: map[string]map[string]any{
			"a-race-thread": threadReadPayload("a-race-thread", "A race thread", firstProject.CanonicalPath, "turn-a", "completed", minimalSelectionSummarySentinel),
		}},
	}
	useCatalogSession(svc, fake)

	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	firstProjectToken := picker.Buttons[0][0].CallbackData
	actions, err := svc.HandleCallback(ctx, 7, 0, 61, 7, firstProjectToken)
	if err != nil {
		t.Fatal(err)
	}
	openToken := minimalThreadPickerCallbackTokenForButton(t, actions.Buttons, "기존 대화 열기")
	page, err := svc.HandleCallback(ctx, 7, 0, 62, 7, openToken)
	if err != nil {
		t.Fatal(err)
	}
	staleAThreadToken := minimalThreadPickerCallbackTokenForButton(t, page.Buttons, "A race thread")

	afterConsume := make(chan struct{})
	releaseA := make(chan struct{})
	var once sync.Once
	svc.afterMinimalPickerConsume = func() {
		once.Do(func() {
			close(afterConsume)
			<-releaseA
		})
	}
	aDone := make(chan struct {
		response *DirectResponse
		err      error
	}, 1)
	go func() {
		response, err := svc.HandleCallback(ctx, 7, 0, 62, 7, staleAThreadToken)
		aDone <- struct {
			response *DirectResponse
			err      error
		}{response: response, err: err}
	}()
	<-afterConsume

	switchPicker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	secondProjectToken := switchPicker.Buttons[1][0].CallbackData
	if _, err := svc.HandleCallback(ctx, 7, 0, 63, 7, secondProjectToken); err != nil {
		t.Fatal(err)
	}
	close(releaseA)
	result := <-aDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.response == nil || !strings.Contains(result.response.Text, "/start") {
		t.Fatalf("post-consume stale A response = %#v, want fail-closed stale guidance", result.response)
	}
	selected, err := svc.store.GetSelectedProject(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if selected != secondProject.ID {
		t.Fatalf("selected project after post-consume race = %q, want %q", selected, secondProject.ID)
	}
	binding, err := svc.store.GetBinding(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("binding after post-consume stale A callback = %#v, want unchanged nil B binding", binding)
	}
	if got := fake.ResumeCalls("a-race-thread"); got != 0 {
		t.Fatalf("resume calls for post-consume stale A thread = %d, want 0", got)
	}
}

func TestSelectMinimalExistingThreadRejectsChildCWD(t *testing.T) {
	svc, _ := newMinimalService(t)
	child := filepath.Join(svc.cfg.Projects[0].CanonicalPath, "child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{stubSession: stubSession{threadReads: map[string]map[string]any{
		"thread-1": threadReadPayload("thread-1", "Child", child, "turn-1", "completed", minimalSelectionSummarySentinel),
	}}}
	useCatalogSession(svc, fake)

	response, err := selectExisting(t, svc, "thread-1", svc.cfg.Projects[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || strings.Contains(response.Text, minimalSelectionSummarySentinel) {
		t.Fatalf("child cwd response = %#v, want safe rejection", response)
	}
	binding, err := svc.store.GetBinding(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if binding != nil {
		t.Fatalf("child cwd binding = %#v, want nil", binding)
	}
}

func TestMinimalExistingThreadCallbackRejectsWrongChatExpiredAndConsumedToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chatID  int64
		topicID int64
		expired bool
		consume bool
	}{
		{name: "wrong chat", chatID: 8},
		{name: "wrong topic", chatID: 7, topicID: 1},
		{name: "expired", chatID: 7, expired: true},
		{name: "consumed", chatID: 7, consume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			ctx := context.Background()
			expires := svc.now().Add(10 * time.Minute)
			if tc.expired {
				expires = svc.now().Add(-time.Minute)
			}
			route := model.MinimalPickerRoute{
				Token:     "11111111111111111111111111111111",
				Action:    minimalExistingSelectAction,
				ProjectID: svc.cfg.Projects[0].ID,
				ThreadID:  "thread-1",
				ChatID:    7,
				TopicID:   0,
				Status:    model.CallbackStatusActive,
				ExpiresAt: model.TimeString(expires.Format(time.RFC3339Nano)),
			}
			if err := svc.store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{route}); err != nil {
				t.Fatal(err)
			}
			if tc.consume {
				if consumed, err := svc.store.ConsumeMinimalPickerRoute(ctx, route.Token, 7, 0, svc.now()); err != nil || consumed == nil {
					t.Fatalf("pre-consume route = %#v err=%v", consumed, err)
				}
			}
			response, err := svc.HandleCallback(ctx, tc.chatID, tc.topicID, 55, 7, route.Token)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || !strings.Contains(response.Text, "/start") {
				t.Fatalf("callback response = %#v, want stale guidance", response)
			}
			binding, err := svc.store.GetBinding(ctx, 7, 0)
			if err != nil {
				t.Fatal(err)
			}
			if binding != nil {
				t.Fatalf("rejected callback stored binding = %#v, want nil", binding)
			}
		})
	}
}

type minimalCatalogSession struct {
	stubSession
	pages                       map[string]map[string]any
	listCalls                   []minimalListCall
	threadReadCalls             []minimalThreadReadCall
	threadReadErrByIncludeTurns map[bool]error
}

type minimalListCall struct {
	limit  int
	cursor string
}

type minimalThreadReadCall struct {
	threadID     string
	includeTurns bool
}

func (s *minimalCatalogSession) ThreadList(ctx context.Context, limit int, cursor string) (map[string]any, error) {
	s.listCalls = append(s.listCalls, minimalListCall{limit: limit, cursor: cursor})
	if payload, ok := s.pages[cursor]; ok {
		return payload, nil
	}
	return threadListPayload(), nil
}

func (s *minimalCatalogSession) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	s.threadReadCalls = append(s.threadReadCalls, minimalThreadReadCall{threadID: threadID, includeTurns: includeTurns})
	s.threadReadID = threadID
	s.threadReadIncludeTurns = includeTurns
	if err := s.threadReadErrByIncludeTurns[includeTurns]; err != nil {
		return nil, err
	}
	if s.threadReadErr != nil {
		return nil, s.threadReadErr
	}
	if payload, ok := s.threadReads[threadID]; ok {
		return payload, nil
	}
	return nil, nil
}

func (s *minimalCatalogSession) ResumeCalls(threadID string) int {
	count := 0
	for _, call := range s.threadResumeCalls {
		if call.threadID == threadID {
			count++
		}
	}
	return count
}

func useCatalogSession(svc *Service, session *minimalCatalogSession) {
	svc.mu.Lock()
	svc.live = session
	svc.liveConnected = true
	svc.mu.Unlock()
}

func selectExisting(t *testing.T, svc *Service, threadID, projectID string) (*DirectResponse, error) {
	t.Helper()
	return svc.selectMinimalExistingThread(context.Background(), 7, 0, &model.MinimalPickerRoute{
		Action:    minimalExistingSelectAction,
		ProjectID: projectID,
		ThreadID:  threadID,
		ChatID:    7,
		TopicID:   0,
		Status:    model.CallbackStatusActive,
		ExpiresAt: model.TimeString(svc.now().Add(10 * time.Minute).Format(time.RFC3339Nano)),
	})
}

func threadReadPayload(id, title, cwd, turnID, status, summary string) map[string]any {
	return map[string]any{
		"thread": map[string]any{
			"id":        id,
			"title":     title,
			"cwd":       cwd,
			"status":    status,
			"updatedAt": int64(1000),
			"turns": []any{map[string]any{
				"id":     turnID,
				"status": status,
				"items": []any{
					map[string]any{"id": "agent-recent", "type": "agentMessage", "text": summary},
					map[string]any{"id": "final-recent", "type": "agentMessage", "phase": "final_answer", "text": summary},
				},
			}},
		},
	}
}

func threadReadSnapshotPayload(id, title, cwd, status string) map[string]any {
	return map[string]any{
		"thread": map[string]any{
			"id":        id,
			"title":     title,
			"cwd":       cwd,
			"status":    status,
			"updatedAt": int64(1000),
		},
	}
}

func assertSQLiteFilesDoNotContain(t *testing.T, svc *Service, sentinel string) error {
	t.Helper()
	for _, path := range []string{svc.cfg.Paths.DBPath, svc.cfg.Paths.DBPath + "-wal", svc.cfg.Paths.DBPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(sentinel)) {
			return fmt.Errorf("sentinel found in %s", filepath.Base(path))
		}
	}
	return nil
}

func rowButtonLabels(rows [][]model.ButtonSpec) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		for _, button := range row {
			out = append(out, button.Text)
		}
	}
	return out
}

func minimalThreadPickerCallbackTokenForButton(t *testing.T, rows [][]model.ButtonSpec, label string) string {
	t.Helper()
	for _, row := range rows {
		for _, button := range row {
			if button.Text == label {
				return button.CallbackData
			}
		}
	}
	t.Fatalf("button %q not found in %#v", label, rows)
	return ""
}

func threadListPayload(items ...map[string]any) map[string]any {
	return threadListPayloadWithCursor("", items...)
}

func threadListPayloadWithCursor(nextCursor string, items ...map[string]any) map[string]any {
	data := make([]any, 0, len(items))
	for _, item := range items {
		data = append(data, item)
	}
	payload := map[string]any{"data": data}
	if nextCursor != "" {
		payload["nextCursor"] = nextCursor
	}
	return payload
}

func threadListItem(id, title, cwd string, updatedAt int64, status, activeTurnID string) map[string]any {
	item := map[string]any{
		"id":        id,
		"title":     title,
		"cwd":       cwd,
		"updatedAt": updatedAt,
		"status":    status,
	}
	if activeTurnID != "" {
		item["activeTurnId"] = activeTurnID
	}
	return item
}

func internalThreadListItem(id, cwd string, updatedAt int64) map[string]any {
	item := threadListItem(id, fmt.Sprintf("Internal %s", id), cwd, updatedAt, "completed", "")
	item["source"] = map[string]any{"subAgent": "memory_consolidation"}
	return item
}
