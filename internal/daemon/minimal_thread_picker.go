package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const (
	minimalExistingPageSize     = 8
	minimalProjectNewAction     = "minimal_project_new"
	minimalExistingOpenAction   = "minimal_existing_open"
	minimalExistingPageAction   = "minimal_existing_page"
	minimalExistingBackAction   = "minimal_existing_back"
	minimalExistingSelectAction = "minimal_existing_select"
)

func (s *Service) minimalProjectActions(ctx context.Context, chatID, topicID int64, project model.Project) (*DirectResponse, error) {
	if err := s.clearMinimalBindingOutsideProject(ctx, chatID, topicID, project.ID); err != nil {
		return nil, err
	}
	newButton, err := s.minimalPickerButton(ctx, chatID, topicID, "새 작업 시작", minimalProjectNewAction, project.ID, "", 0)
	if err != nil {
		return nil, err
	}
	openButton, err := s.minimalPickerButton(ctx, chatID, topicID, "기존 대화 열기", minimalExistingOpenAction, project.ID, "", 0)
	if err != nil {
		return nil, err
	}
	return &DirectResponse{
		Text:    "작업폴더: " + project.DisplayName + "\n[새 작업 시작] [기존 대화 열기]",
		Buttons: [][]model.ButtonSpec{{newButton, openButton}},
	}, nil
}

func (s *Service) minimalExistingThreadPage(ctx context.Context, chatID, topicID int64, projectID string, page int) (*DirectResponse, error) {
	if page < 0 {
		page = 0
	}
	project, err := s.projectRegistry.Resolve(projectID)
	if err != nil {
		return &DirectResponse{Text: "등록되지 않은 작업폴더입니다. /start를 다시 보내주세요.", CallbackText: "등록되지 않은 작업폴더입니다."}, nil
	}
	session, err := s.minimalCatalogSession()
	if err != nil {
		return nil, err
	}
	threads, err := s.minimalAllProjectThreads(ctx, session, project)
	if err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		retry, retryErr := s.minimalPickerButton(ctx, chatID, topicID, "다시 시도", minimalExistingOpenAction, project.ID, "", 0)
		if retryErr != nil {
			return nil, retryErr
		}
		back, backErr := s.minimalPickerButton(ctx, chatID, topicID, "뒤로", minimalExistingBackAction, project.ID, "", 0)
		if backErr != nil {
			return nil, backErr
		}
		return &DirectResponse{
			Text:    "열 수 있는 기존 대화가 없습니다. 잠시 후 다시 시도하거나 뒤로 가세요.",
			Buttons: [][]model.ButtonSpec{{retry, back}},
		}, nil
	}
	start := page * minimalExistingPageSize
	if start >= len(threads) {
		page = (len(threads) - 1) / minimalExistingPageSize
		start = page * minimalExistingPageSize
	}
	end := min(start+minimalExistingPageSize, len(threads))
	buttons := make([][]model.ButtonSpec, 0, end-start+1)
	for _, thread := range threads[start:end] {
		button, err := s.minimalPickerButton(ctx, chatID, topicID, minimalThreadRowLabel(thread), minimalExistingSelectAction, project.ID, thread.ID, page)
		if err != nil {
			return nil, err
		}
		buttons = append(buttons, []model.ButtonSpec{button})
	}
	nav := []model.ButtonSpec{}
	if page > 0 {
		button, err := s.minimalPickerButton(ctx, chatID, topicID, "이전", minimalExistingPageAction, project.ID, "", page-1)
		if err != nil {
			return nil, err
		}
		nav = append(nav, button)
	}
	if end < len(threads) {
		button, err := s.minimalPickerButton(ctx, chatID, topicID, "다음", minimalExistingPageAction, project.ID, "", page+1)
		if err != nil {
			return nil, err
		}
		nav = append(nav, button)
	}
	back, err := s.minimalPickerButton(ctx, chatID, topicID, "뒤로", minimalExistingBackAction, project.ID, "", 0)
	if err != nil {
		return nil, err
	}
	nav = append(nav, back)
	buttons = append(buttons, nav)
	return &DirectResponse{
		Text:    fmt.Sprintf("작업폴더: %s\n기존 대화를 선택하세요. (%d/%d)", project.DisplayName, page+1, (len(threads)+minimalExistingPageSize-1)/minimalExistingPageSize),
		Buttons: buttons,
	}, nil
}

func (s *Service) handleMinimalPickerRoute(ctx context.Context, chatID, topicID, messageID int64, route *model.MinimalPickerRoute) (*DirectResponse, error) {
	if route == nil {
		return &DirectResponse{Text: "이 버튼은 만료되었습니다. /start를 다시 보내주세요.", CallbackText: "만료된 버튼입니다."}, nil
	}
	project, err := s.projectRegistry.Resolve(route.ProjectID)
	if err != nil {
		return &DirectResponse{Text: "등록되지 않은 작업폴더입니다. /start를 다시 보내주세요.", CallbackText: "등록되지 않은 작업폴더입니다."}, nil
	}
	switch route.Action {
	case minimalProjectNewAction:
		unlockSelection := minimalSelectionLock(chatID, topicID)
		defer unlockSelection()
		if err := s.store.SetSelectedProject(ctx, chatID, topicID, project.ID); err != nil {
			return nil, err
		}
		if err := s.store.ClearBinding(ctx, chatID, topicID); err != nil {
			return nil, err
		}
		return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, "새 작업을 시작합니다.", func(context.Context) (*DirectResponse, error) {
			return &DirectResponse{Text: "작업폴더: " + project.DisplayName + "\n자연어로 작업을 보내세요."}, nil
		})
	case minimalExistingOpenAction, minimalExistingPageAction:
		unlockSelection := minimalSelectionLock(chatID, topicID)
		defer unlockSelection()
		current, err := s.minimalPickerRouteMatchesCurrentProject(ctx, chatID, topicID, route)
		if err != nil {
			return nil, err
		}
		if !current {
			return minimalPickerStaleProjectResponse(), nil
		}
		return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, "기존 대화 목록입니다.", func(context.Context) (*DirectResponse, error) {
			return s.minimalExistingThreadPage(ctx, chatID, topicID, project.ID, route.Page)
		})
	case minimalExistingBackAction:
		unlockSelection := minimalSelectionLock(chatID, topicID)
		defer unlockSelection()
		current, err := s.minimalPickerRouteMatchesCurrentProject(ctx, chatID, topicID, route)
		if err != nil {
			return nil, err
		}
		if !current {
			return minimalPickerStaleProjectResponse(), nil
		}
		return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, "작업폴더 메뉴입니다.", func(context.Context) (*DirectResponse, error) {
			return s.minimalProjectActions(ctx, chatID, topicID, project)
		})
	case minimalExistingSelectAction:
		unlockSelection := minimalSelectionLock(chatID, topicID)
		defer unlockSelection()
		current, err := s.minimalPickerRouteMatchesCurrentProject(ctx, chatID, topicID, route)
		if err != nil {
			return nil, err
		}
		if !current {
			return minimalPickerStaleProjectResponse(), nil
		}
		response, stale, err := s.selectMinimalExistingThreadResult(ctx, chatID, topicID, route)
		if err != nil {
			return nil, err
		}
		if stale {
			return s.refreshMinimalExistingThreadPageAfterStaleSelection(ctx, chatID, topicID, messageID, route, response)
		}
		return response, nil
	default:
		return &DirectResponse{Text: "이 버튼은 유효하지 않습니다. /start를 다시 보내주세요.", CallbackText: "유효하지 않은 버튼입니다."}, nil
	}
}

func (s *Service) minimalPickerRouteMatchesCurrentProject(ctx context.Context, chatID, topicID int64, route *model.MinimalPickerRoute) (bool, error) {
	if route == nil {
		return false, nil
	}
	switch route.Action {
	case minimalExistingOpenAction, minimalExistingPageAction, minimalExistingBackAction, minimalExistingSelectAction:
	default:
		return true, nil
	}
	selectedProjectID, err := s.store.GetSelectedProject(ctx, chatID, topicID)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(selectedProjectID) == strings.TrimSpace(route.ProjectID), nil
}

func minimalPickerStaleProjectResponse() *DirectResponse {
	return &DirectResponse{Text: "이 버튼은 만료되었습니다. /start를 다시 보내주세요.", CallbackText: "만료된 버튼입니다."}
}

func (s *Service) selectMinimalExistingThread(ctx context.Context, chatID, topicID int64, route *model.MinimalPickerRoute) (*DirectResponse, error) {
	response, _, err := s.selectMinimalExistingThreadResult(ctx, chatID, topicID, route)
	return response, err
}

func (s *Service) selectMinimalExistingThreadResult(ctx context.Context, chatID, topicID int64, route *model.MinimalPickerRoute) (*DirectResponse, bool, error) {
	if route == nil || route.Action != minimalExistingSelectAction || strings.TrimSpace(route.ThreadID) == "" {
		return minimalExistingRetryResponse(), true, nil
	}
	project, err := s.projectRegistry.Resolve(route.ProjectID)
	if err != nil {
		return &DirectResponse{Text: "등록되지 않은 작업폴더입니다. /start를 다시 보내주세요.", CallbackText: "등록되지 않은 작업폴더입니다."}, false, nil
	}
	session, err := s.minimalCatalogSession()
	if err != nil {
		return nil, false, err
	}
	read, ok, err := s.readMinimalExistingThread(ctx, session, project, route.ThreadID)
	if err != nil || !ok {
		return minimalExistingRetryResponse(), true, nil
	}
	thread := read.thread
	snapshot := read.snapshot
	if err := s.markPCOriginThreadFromPayload(ctx, thread.ID, read.payload); err != nil {
		return nil, false, err
	}
	if err := s.persistMinimalExistingThreadBinding(ctx, chatID, topicID, project, thread, snapshot); err != nil {
		return nil, false, err
	}
	return &DirectResponse{
		Text:         minimalExistingThreadSummary(project, thread, snapshot),
		CallbackText: "기존 대화를 열었습니다.",
		ThreadID:     thread.ID,
		TurnID:       snapshot.LatestTurnID,
	}, false, nil
}

func (s *Service) refreshMinimalExistingThreadPageAfterStaleSelection(ctx context.Context, chatID, topicID, messageID int64, route *model.MinimalPickerRoute, staleResponse *DirectResponse) (*DirectResponse, error) {
	callbackText := "다시 시도해 주세요."
	if staleResponse != nil && strings.TrimSpace(staleResponse.CallbackText) != "" {
		callbackText = staleResponse.CallbackText
	}
	return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, callbackText, func(context.Context) (*DirectResponse, error) {
		response, err := s.minimalExistingThreadPage(ctx, chatID, topicID, route.ProjectID, route.Page)
		if err != nil {
			return nil, err
		}
		return minimalExistingRetryPageResponse(response), nil
	})
}

func (s *Service) minimalBoundThreadForProject(ctx context.Context, chatID, topicID int64, projectID string) (*model.Thread, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, nil
	}
	selectedProjectID, err := s.store.GetSelectedProject(ctx, chatID, topicID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(selectedProjectID) != projectID {
		return nil, nil
	}
	project, err := s.projectRegistry.Resolve(projectID)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.GetBinding(ctx, chatID, topicID)
	if err != nil || binding == nil || strings.TrimSpace(binding.ThreadID) == "" {
		return nil, err
	}
	session, err := s.minimalCatalogSession()
	if err != nil {
		return nil, err
	}
	explicitLinked, err := s.minimalLinkedChildForBinding(ctx, chatID, topicID, project, binding.ThreadID)
	if err != nil {
		return nil, err
	}
	read, ok, err := s.readMinimalExistingThread(ctx, session, project, binding.ThreadID)
	if err != nil {
		return nil, err
	}
	if explicitLinked != nil && !ok {
		return nil, errors.New("canonical linked Codex thread is unavailable")
	}
	if existing, loadErr := s.store.GetThread(ctx, binding.ThreadID); loadErr != nil {
		return nil, loadErr
	} else if existing != nil && staleMinimalTerminalRefresh(*existing, read.snapshot) {
		return existing, nil
	}
	if !ok {
		if err := s.store.ClearBinding(ctx, chatID, topicID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	thread := read.thread
	if err := s.persistThread(ctx, thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func (s *Service) minimalLinkedChildForBinding(ctx context.Context, chatID, topicID int64, project model.Project, boundThreadID string) (*model.MinimalLinkedThread, error) {
	boundThreadID = strings.TrimSpace(boundThreadID)
	if boundThreadID == "" {
		return nil, nil
	}
	link, err := s.store.GetMinimalLinkedThreadByLinkedID(ctx, boundThreadID)
	if err != nil || link == nil {
		return link, err
	}
	if link.ChatID != chatID || link.TopicID != topicID || strings.TrimSpace(link.ProjectID) != project.ID {
		return nil, errors.New("canonical linked binding belongs to another chat or project")
	}
	if strings.TrimSpace(link.LinkedThreadID) == "" {
		return nil, errors.New("canonical linked Codex thread is unavailable")
	}
	return link, nil
}

type minimalExistingThreadRead struct {
	thread   model.Thread
	snapshot appserver.ThreadReadSnapshot
	payload  map[string]any
}

func (s *Service) readMinimalExistingThread(ctx context.Context, session Session, project model.Project, threadID string) (minimalExistingThreadRead, bool, error) {
	threadID = strings.TrimSpace(threadID)
	if session == nil || threadID == "" {
		return minimalExistingThreadRead{}, false, nil
	}
	payload, err := session.ThreadRead(ctx, threadID, true)
	if err != nil && isPaginatedThreadReadUnsupported(err) {
		payload, err = session.ThreadRead(ctx, threadID, false)
	}
	if err != nil {
		return minimalExistingThreadRead{}, false, err
	}
	if len(payload) == 0 {
		return minimalExistingThreadRead{}, false, nil
	}
	snapshot := appserver.SnapshotFromThreadRead(payload)
	thread := snapshot.Thread
	if strings.TrimSpace(thread.ID) == "" || thread.ID != threadID {
		return minimalExistingThreadRead{}, false, nil
	}
	matched, ok := s.projectRegistry.MatchExactCWD(thread.CWD)
	if !ok || matched.ID != project.ID {
		return minimalExistingThreadRead{}, false, nil
	}
	thread.CWD = project.CanonicalPath
	thread.ProjectName = project.DisplayName
	if snapshot.LatestTurnStatus != "" {
		thread.Status = canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	}
	if isTerminalStatus(thread.Status) {
		thread.ActiveTurnID = ""
	} else if snapshot.LatestTurnID != "" {
		thread.ActiveTurnID = snapshot.LatestTurnID
	}
	return minimalExistingThreadRead{thread: thread, snapshot: snapshot, payload: payload}, true, nil
}

func (s *Service) persistMinimalExistingThreadBinding(ctx context.Context, chatID, topicID int64, project model.Project, thread model.Thread, snapshot appserver.ThreadReadSnapshot) error {
	if err := s.persistThread(ctx, thread); err != nil {
		return err
	}
	if err := s.store.SetSelectedProject(ctx, chatID, topicID, project.ID); err != nil {
		return err
	}
	if err := s.store.SetBinding(ctx, chatID, topicID, thread.ID, model.BindingModeBound); err != nil {
		return err
	}
	observed := thread
	if snapshot.LatestTurnID != "" {
		observed.ActiveTurnID = snapshot.LatestTurnID
	}
	if snapshot.LatestTurnStatus != "" {
		observed.Status = canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	}
	return s.store.ObserveMinimalThread(ctx, observed, project.ID, s.now())
}

func (s *Service) minimalAllProjectThreads(ctx context.Context, session Session, project model.Project) ([]model.Thread, error) {
	cursor := ""
	seenCursors := map[string]struct{}{}
	threads := []model.Thread{}
	for {
		page, err := s.minimalProjectThreads(ctx, session, project, cursor)
		if err != nil {
			return nil, err
		}
		threads = append(threads, page.Threads...)
		if page.NextCursor == "" {
			break
		}
		if _, seen := seenCursors[page.NextCursor]; seen {
			return nil, errors.New("app-server thread list returned a repeated cursor")
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	sortMinimalThreads(threads)
	return threads, nil
}

func (s *Service) minimalPickerButton(ctx context.Context, chatID, topicID int64, label, action, projectID, threadID string, page int) (model.ButtonSpec, error) {
	token := randomToken()
	route := model.MinimalPickerRoute{
		Token:     token,
		Action:    action,
		ProjectID: projectID,
		ThreadID:  threadID,
		Page:      page,
		ChatID:    chatID,
		TopicID:   topicID,
		Status:    model.CallbackStatusActive,
		ExpiresAt: model.TimeString(s.now().UTC().Add(minimalCallbackTTL).Format(time.RFC3339Nano)),
	}
	if err := s.store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{route}); err != nil {
		return model.ButtonSpec{}, err
	}
	return model.ButtonSpec{Text: label, CallbackData: token}, nil
}

func (s *Service) clearMinimalBindingOutsideProject(ctx context.Context, chatID, topicID int64, projectID string) error {
	binding, err := s.store.GetBinding(ctx, chatID, topicID)
	if err != nil || binding == nil {
		return err
	}
	thread, err := s.store.GetThread(ctx, binding.ThreadID)
	if err != nil {
		return err
	}
	if thread != nil {
		if project, ok := s.projectRegistry.MatchExactCWD(thread.CWD); ok && project.ID == projectID {
			return nil
		}
	}
	return s.store.ClearBinding(ctx, chatID, topicID)
}

func (s *Service) minimalCatalogSession() (Session, error) {
	s.mu.RLock()
	live, liveConnected := s.live, s.liveConnected
	poll, pollConnected := s.poll, s.pollConnected
	s.mu.RUnlock()
	if liveConnected && live != nil {
		return live, nil
	}
	if pollConnected && poll != nil {
		return poll, nil
	}
	return nil, errors.New("app-server session is not ready")
}

func minimalThreadRowLabel(thread model.Thread) string {
	label := strings.TrimSpace(thread.Title)
	if label == "" {
		label = thread.ShortID()
	}
	return boundRunes(label, 64)
}

func minimalExistingRetryResponse() *DirectResponse {
	return &DirectResponse{
		Text:         "이 대화를 열 수 없습니다. 목록을 새로고침한 뒤 다시 선택해 주세요.",
		CallbackText: "다시 시도해 주세요.",
	}
}

func isPaginatedThreadReadUnsupported(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "paginated_threads is not supported yet")
}

func minimalExistingRetryPageResponse(response *DirectResponse) *DirectResponse {
	retry := minimalExistingRetryResponse()
	if response == nil {
		return retry
	}
	if strings.TrimSpace(response.Text) == "" {
		response.Text = retry.Text
		return response
	}
	if !strings.Contains(response.Text, retry.Text) {
		response.Text = retry.Text + "\n\n" + response.Text
	}
	return response
}

func minimalExistingThreadSummary(project model.Project, thread model.Thread, snapshot appserver.ThreadReadSnapshot) string {
	title := cleanMinimalDisplayText(thread.Title)
	if title == "" || title == thread.ID {
		title = thread.ShortID()
	}
	state := cleanMinimalDisplayText(snapshot.LatestTurnStatus)
	if state == "" {
		state = cleanMinimalDisplayText(thread.Status)
	}
	if state == "" {
		state = "idle"
	}
	summary := strings.TrimSpace(snapshot.LatestFinalText)
	if summary == "" {
		for i := len(snapshot.LatestAgentMessages) - 1; i >= 0; i-- {
			if strings.TrimSpace(snapshot.LatestAgentMessages[i]) != "" {
				summary = strings.TrimSpace(snapshot.LatestAgentMessages[i])
				break
			}
		}
	}
	if summary == "" {
		summary = "표시할 최근 요약이 없습니다."
	}
	lines := []string{
		"작업폴더: " + project.DisplayName,
		"대화: " + title,
		"상태: " + state,
		"",
		"최근 요약",
		boundRunes(cleanMinimalDisplayText(summary), 900),
	}
	return strings.Join(lines, "\n")
}

func cleanMinimalDisplayText(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "<nil>", ""))
	return strings.Join(strings.Fields(value), " ")
}

func boundRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if limit <= 0 || len(runes) <= limit {
		return string(runes)
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "..."
}
