package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const minimalThreadCatalogLimit = 100

type minimalThreadPage struct {
	Threads    []model.Thread
	NextCursor string
}

func (s *Service) minimalProjectThreads(ctx context.Context, session Session, project model.Project, cursor string) (minimalThreadPage, error) {
	if session == nil {
		return minimalThreadPage{}, errors.New("app-server session is unavailable")
	}
	result, err := session.ThreadList(ctx, minimalThreadCatalogLimit, strings.TrimSpace(cursor))
	if err != nil {
		return minimalThreadPage{}, err
	}
	out := minimalThreadPage{NextCursor: minimalListNextCursor(result)}
	for _, thread := range appserver.ThreadsFromList(result) {
		if thread.IsInternal() {
			continue
		}
		matched, ok := s.projectRegistry.MatchExactCWD(thread.CWD)
		if !ok || matched.ID != project.ID {
			continue
		}
		thread.CWD = matched.CanonicalPath
		thread.ProjectName = matched.DisplayName
		out.Threads = append(out.Threads, thread)
	}
	sortMinimalThreads(out.Threads)
	return out, nil
}

func sortMinimalThreads(threads []model.Thread) {
	sort.SliceStable(threads, func(i, j int) bool {
		leftActive := threadLooksActiveForPolling(threads[i])
		rightActive := threadLooksActiveForPolling(threads[j])
		if leftActive != rightActive {
			return leftActive
		}
		if threads[i].UpdatedAt != threads[j].UpdatedAt {
			return threads[i].UpdatedAt > threads[j].UpdatedAt
		}
		return threads[i].ID < threads[j].ID
	})
}

func minimalListNextCursor(result map[string]any) string {
	for _, key := range []string{"nextCursor", "next_cursor", "cursor"} {
		if value, ok := result[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
