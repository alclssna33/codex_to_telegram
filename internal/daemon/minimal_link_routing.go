package daemon

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type minimalLinkedRouteDecision struct {
	ThreadID       string
	SourceThreadID string
	SourceTurnID   string
	Divergent      bool
}

const (
	minimalTurnAtOrBefore = "at_or_before"
	minimalTurnAfter      = "after"
)

var errMinimalLinkedSourceDiverged = errors.New("minimal linked source has diverged")

func minimalSourceTurnRelation(payload map[string]any, anchorTurnID, routedTurnID string) (string, error) {
	anchorTurnID = strings.TrimSpace(anchorTurnID)
	routedTurnID = strings.TrimSpace(routedTurnID)
	if anchorTurnID == "" || routedTurnID == "" {
		return "", errSourceTurnUnavailable
	}
	positions := map[string][]int{}
	next := 0
	root := payload
	if nested := payloadMapAny(payload["thread"]); len(nested) > 0 {
		root = nested
	}
	collectMinimalTurnPositions(root, positions, &next)
	anchorPositions := positions[anchorTurnID]
	routedPositions := positions[routedTurnID]
	if len(anchorPositions) != 1 || len(routedPositions) != 1 {
		return "", errSourceTurnUnavailable
	}
	if routedPositions[0] <= anchorPositions[0] {
		return minimalTurnAtOrBefore, nil
	}
	return minimalTurnAfter, nil
}

func collectMinimalTurnPositions(value any, positions map[string][]int, next *int) {
	switch typed := value.(type) {
	case map[string]any:
		if turns, ok := typed["turns"]; ok {
			collectMinimalTurnArrayPositions(turns, positions, next)
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if key != "turns" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectMinimalTurnPositions(typed[key], positions, next)
		}
	case []any:
		for _, child := range typed {
			collectMinimalTurnPositions(child, positions, next)
		}
	case []map[string]any:
		for _, child := range typed {
			collectMinimalTurnPositions(child, positions, next)
		}
	}
}

func collectMinimalTurnArrayPositions(value any, positions map[string][]int, next *int) {
	switch turns := value.(type) {
	case []any:
		for _, raw := range turns {
			turn, _ := raw.(map[string]any)
			recordMinimalTurnPosition(turn, positions, next)
			collectMinimalTurnPositions(turn, positions, next)
		}
	case []map[string]any:
		for _, turn := range turns {
			recordMinimalTurnPosition(turn, positions, next)
			collectMinimalTurnPositions(turn, positions, next)
		}
	}
}

func recordMinimalTurnPosition(turn map[string]any, positions map[string][]int, next *int) {
	if turn == nil {
		return
	}
	id := strings.TrimSpace(payloadString(turn["id"]))
	if id == "" {
		return
	}
	positions[id] = append(positions[id], *next)
	*next = *next + 1
}

func (r *Router) resolveMinimalLinkedRoute(ctx context.Context, inbound model.InboundText, threadID, turnID string, payload map[string]any) (minimalLinkedRouteDecision, error) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	decision := minimalLinkedRouteDecision{ThreadID: threadID, SourceThreadID: threadID, SourceTurnID: turnID}
	if r == nil || r.service == nil || r.service.store == nil || threadID == "" {
		return decision, nil
	}
	if link, err := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, threadID); err != nil {
		return decision, err
	} else if link != nil && link.ChatID == inbound.ChatID && link.TopicID == inbound.TopicID {
		decision.SourceThreadID = strings.TrimSpace(link.SourceThreadID)
		return decision, nil
	}
	link, err := r.service.store.GetMinimalLinkedThread(ctx, inbound.ChatID, inbound.TopicID, threadID)
	if err != nil {
		return decision, err
	}
	if link == nil || strings.TrimSpace(link.LinkedThreadID) == "" {
		return decision, nil
	}
	decision.ThreadID = strings.TrimSpace(link.LinkedThreadID)
	decision.SourceThreadID = strings.TrimSpace(link.SourceThreadID)
	decision.SourceTurnID = strings.TrimSpace(link.SourceAnchorTurnID)
	relation, err := minimalSourceTurnRelation(payload, link.SourceAnchorTurnID, turnID)
	if err != nil {
		return decision, err
	}
	if relation == minimalTurnAfter {
		decision.Divergent = true
		return decision, errMinimalLinkedSourceDiverged
	}
	return decision, nil
}

func (r *Router) minimalRouteMayHaveCanonicalLink(ctx context.Context, inbound model.InboundText, threadID string) (bool, error) {
	threadID = strings.TrimSpace(threadID)
	if r == nil || r.service == nil || r.service.store == nil || threadID == "" {
		return false, nil
	}
	if link, err := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, threadID); err != nil {
		return false, err
	} else if link != nil && link.ChatID == inbound.ChatID && link.TopicID == inbound.TopicID {
		return true, nil
	}
	link, err := r.service.store.GetMinimalLinkedThread(ctx, inbound.ChatID, inbound.TopicID, threadID)
	if err != nil {
		return false, err
	}
	return link != nil && strings.TrimSpace(link.LinkedThreadID) != "", nil
}

func minimalLinkedSourceDivergedResponseText() string {
	return "원본 Codex 작업이 텔레그램 연동 이후에 더 진행되어, 이 답글은 자동으로 합칠 수 없습니다.\n\nCodex에서 텔레그램 연동 작업을 열어 이어서 작업하거나, 텔레그램에서는 연동된 작업 알림에 답장해 주세요."
}

func minimalLinkedSourceUnavailableResponseText() string {
	return "원본 Codex 작업의 기준 답변을 확인할 수 없어, 이 답글은 안전하게 이어갈 수 없습니다.\n\nCodex에서 텔레그램 연동 작업을 열어 이어서 작업하거나, 텔레그램에서는 연동된 작업 알림에 답장해 주세요."
}
