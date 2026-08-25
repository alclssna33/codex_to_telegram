package daemon

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestMinimalProjectThreadsUsesExactCWDActiveThenUpdatedStableOrder(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	projectOne := svc.cfg.Projects[0]
	child := filepath.Join(projectOne.CanonicalPath, "child")
	siblingPrefix := projectOne.CanonicalPath + "-sibling"
	for _, path := range []string{child, siblingPrefix} {
		if err := ensureTestDir(path); err != nil {
			t.Fatal(err)
		}
	}

	fake := &minimalCatalogSession{pages: map[string]map[string]any{
		"": threadListPayload(
			threadListItem("idle-old", "Idle old", projectOne.CanonicalPath, 100, "completed", ""),
			threadListItem("active-old", "Active old", projectOne.CanonicalPath, 200, "inProgress", "turn-active-old"),
			threadListItem("other-project", "Other project", svc.cfg.Projects[1].CanonicalPath, 900, "inProgress", "turn-other"),
			threadListItem("child-thread", "Child", child, 800, "completed", ""),
			threadListItem("sibling-prefix", "Sibling prefix", siblingPrefix, 700, "completed", ""),
			internalThreadListItem("internal-thread", projectOne.CanonicalPath, 950),
			threadListItem("idle-new", "Idle new", projectOne.CanonicalPath, 300, "completed", ""),
			threadListItem("active-new", "Active new", projectOne.CanonicalPath, 300, "inProgress", "turn-active-new"),
		),
	}}

	page, err := svc.minimalProjectThreads(ctx, fake, projectOne, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := threadIDs(page.Threads), []string{"active-new", "active-old", "idle-new", "idle-old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread order = %v, want %v", got, want)
	}
	if len(fake.listCalls) != 1 || fake.listCalls[0].limit > 100 || fake.listCalls[0].cursor != "" {
		t.Fatalf("ThreadList calls = %#v, want one bounded first-page call", fake.listCalls)
	}
}

func threadIDs(threads []model.Thread) []string {
	out := make([]string, 0, len(threads))
	for _, thread := range threads {
		out = append(out, thread.ID)
	}
	return out
}
