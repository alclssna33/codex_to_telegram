package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type contextCloseSession interface {
	CloseContext(context.Context) error
}

type minimalLinkWorker struct {
	RegistryKey     string
	ThreadID        string
	Generation      uint64
	SessionIdentity string
	Session         continuationSession
	Events          <-chan control.Event
	Context         context.Context
	cancel          context.CancelFunc
	loopOnce        sync.Once
}

type minimalLinkWorkerLogger func(string, lifecycleFields)
type minimalLinkWorkerAcquireHook func(*minimalLinkWorker)

type minimalLinkConfirmedRelease struct {
	generation      uint64
	sessionIdentity string
}

type minimalLinkWorkerManager struct {
	mu            sync.Mutex
	factory       func() continuationSession
	closeTimeout  time.Duration
	log           minimalLinkWorkerLogger
	onAcquire     minimalLinkWorkerAcquireHook
	identityNonce string

	nextGeneration   uint64
	closed           bool
	acquireWG        sync.WaitGroup
	acquireCloseErrs []error
	byKey            map[string]*minimalLinkWorker
	byThread         map[string]*minimalLinkWorker
	byIdentity       map[string]*minimalLinkWorker
	confirmedRelease map[string]minimalLinkConfirmedRelease
	busyKeys         map[string]struct{}
	busyThreads      map[string]struct{}
}

var minimalLinkWorkerNonceFallback uint64

func newMinimalLinkWorkerManager(factory func() continuationSession, closeTimeout time.Duration, log minimalLinkWorkerLogger) *minimalLinkWorkerManager {
	if closeTimeout <= 0 {
		closeTimeout = 30 * time.Second
	}
	return &minimalLinkWorkerManager{
		factory:          factory,
		closeTimeout:     closeTimeout,
		log:              log,
		identityNonce:    newMinimalLinkWorkerIdentityNonce(),
		byKey:            map[string]*minimalLinkWorker{},
		byThread:         map[string]*minimalLinkWorker{},
		byIdentity:       map[string]*minimalLinkWorker{},
		confirmedRelease: map[string]minimalLinkConfirmedRelease{},
		busyKeys:         map[string]struct{}{},
		busyThreads:      map[string]struct{}{},
	}
}

func newMinimalLinkWorkerIdentityNonce() string {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err == nil {
		return hex.EncodeToString(nonce[:])
	}
	fallback := atomic.AddUint64(&minimalLinkWorkerNonceFallback, 1)
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), fallback)
}

func (m *minimalLinkWorkerManager) Acquire(ctx context.Context, registryKey, linkedID string) (*minimalLinkWorker, error) {
	if m == nil {
		return nil, errors.New("minimal link worker manager is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	registryKey = strings.TrimSpace(registryKey)
	linkedID = strings.TrimSpace(linkedID)
	if registryKey == "" {
		return nil, errors.New("minimal link worker registry key is required")
	}
	if err := m.beginAcquire(registryKey, linkedID); err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			m.finishAcquire(registryKey, linkedID)
		}
	}()

	session := m.factorySession()
	if session == nil {
		return nil, errors.New("minimal link worker factory returned nil session")
	}
	if err := session.Start(ctx); err != nil {
		m.closeProvisionalSession(ctx, session)
		return nil, fmt.Errorf("start minimal link worker: %w", err)
	}
	events := session.Subscribe()
	if events == nil {
		m.closeProvisionalSession(ctx, session)
		return nil, errors.New("minimal link worker subscription is unavailable")
	}
	workerCtx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		m.closeProvisionalSession(ctx, session)
		return nil, errors.New("minimal link worker manager is shutting down")
	}
	if existing := m.byKey[registryKey]; existing != nil {
		m.mu.Unlock()
		cancel()
		m.closeProvisionalSession(ctx, session)
		return nil, fmt.Errorf("minimal link worker key %q is already active", registryKey)
	}
	if linkedID != "" {
		if existing := m.byThread[linkedID]; existing != nil {
			m.mu.Unlock()
			cancel()
			m.closeProvisionalSession(ctx, session)
			return nil, fmt.Errorf("minimal link worker thread %q is already active", linkedID)
		}
	}
	m.nextGeneration++
	generation := m.nextGeneration
	worker := &minimalLinkWorker{
		RegistryKey:     registryKey,
		ThreadID:        linkedID,
		Generation:      generation,
		SessionIdentity: fmt.Sprintf("minimal-link-worker:%s:%d", m.identityNonce, generation),
		Session:         session,
		Events:          events,
		Context:         workerCtx,
		cancel:          cancel,
	}
	m.byKey[registryKey] = worker
	if linkedID != "" {
		m.byThread[linkedID] = worker
		delete(m.confirmedRelease, linkedID)
	}
	m.byIdentity[worker.SessionIdentity] = worker
	m.finishAcquireLocked(registryKey, linkedID)
	onAcquire := m.onAcquire
	m.mu.Unlock()
	finished = true

	m.logEvent("minimal_link_worker_started", worker, nil)
	if onAcquire != nil {
		onAcquire(worker)
	}
	return worker, nil
}

func (m *minimalLinkWorkerManager) SetAcquireHook(hook minimalLinkWorkerAcquireHook) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAcquire = hook
}

func (m *minimalLinkWorkerManager) BindThread(worker *minimalLinkWorker, threadID string) error {
	if m == nil {
		return errors.New("minimal link worker manager is unavailable")
	}
	if worker == nil {
		return errors.New("minimal link worker is required")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return errors.New("minimal link worker thread id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("minimal link worker manager is shutting down")
	}
	current := m.byKey[worker.RegistryKey]
	if current == nil || current.Generation != worker.Generation || current.SessionIdentity != worker.SessionIdentity {
		return errors.New("minimal link worker generation is no longer active")
	}
	if current.ThreadID == threadID {
		return nil
	}
	if current.ThreadID != "" {
		return errors.New("minimal link worker is already bound to a different thread")
	}
	if existing := m.byThread[threadID]; existing != nil && existing.Generation != worker.Generation {
		return fmt.Errorf("minimal link worker thread %q is already active", threadID)
	}
	if _, busy := m.busyThreads[threadID]; busy {
		return fmt.Errorf("minimal link worker thread %q is busy", threadID)
	}
	current.ThreadID = threadID
	m.byThread[threadID] = current
	delete(m.confirmedRelease, threadID)
	return nil
}

func (m *minimalLinkWorkerManager) ByThread(threadID string) (*minimalLinkWorker, bool) {
	if m == nil {
		return nil, false
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	worker, ok := m.byThread[threadID]
	return worker, ok
}

func (m *minimalLinkWorkerManager) BySessionIdentity(identity string) (*minimalLinkWorker, bool) {
	if m == nil {
		return nil, false
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	worker, ok := m.byIdentity[identity]
	return worker, ok
}

func (m *minimalLinkWorkerManager) Release(ctx context.Context, threadID string, generation uint64) (bool, error) {
	if m == nil {
		return false, nil
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false, nil
	}
	m.mu.Lock()
	worker := m.byThread[threadID]
	if worker == nil || worker.Generation != generation {
		m.mu.Unlock()
		return false, nil
	}
	m.removeLocked(worker)
	m.mu.Unlock()

	m.logEvent("minimal_link_release_started", worker, nil)
	err := m.closeWorker(ctx, worker)
	if err != nil {
		if appserver.CommandExitConfirmed(err) {
			m.recordConfirmedRelease(worker.ThreadID, worker.Generation, worker.SessionIdentity)
			m.logEvent("minimal_link_release_confirmed_after_exit", worker, err)
			return true, nil
		}
		m.logEvent("minimal_link_release_failed", worker, err)
		return true, err
	}
	m.recordConfirmedRelease(worker.ThreadID, worker.Generation, worker.SessionIdentity)
	m.logEvent("minimal_link_worker_closed", worker, nil)
	return true, nil
}

func (m *minimalLinkWorkerManager) ConfirmedRelease(threadID string, generation uint64) bool {
	if m == nil || generation == 0 {
		return false
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	confirmed, ok := m.confirmedRelease[threadID]
	return ok && confirmed.generation == generation
}

func (m *minimalLinkWorkerManager) ConfirmedReleaseIdentity(threadID string, generation uint64) (string, bool) {
	if m == nil || generation == 0 {
		return "", false
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	confirmed, ok := m.confirmedRelease[threadID]
	if !ok || confirmed.generation != generation {
		return "", false
	}
	return confirmed.sessionIdentity, strings.TrimSpace(confirmed.sessionIdentity) != ""
}

func (m *minimalLinkWorkerManager) ForgetConfirmedRelease(threadID string, generation uint64) {
	if m == nil || generation == 0 {
		return
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forgetConfirmedReleaseLocked(threadID, generation)
}

func (m *minimalLinkWorkerManager) CloseAll(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.acquireWG.Wait()

	m.mu.Lock()
	acquireCloseErrs := append([]error(nil), m.acquireCloseErrs...)
	m.acquireCloseErrs = nil
	workers := make([]*minimalLinkWorker, 0, len(m.byIdentity))
	for _, worker := range m.byIdentity {
		workers = append(workers, worker)
	}
	m.byKey = map[string]*minimalLinkWorker{}
	m.byThread = map[string]*minimalLinkWorker{}
	m.byIdentity = map[string]*minimalLinkWorker{}
	m.confirmedRelease = map[string]minimalLinkConfirmedRelease{}
	m.busyKeys = map[string]struct{}{}
	m.busyThreads = map[string]struct{}{}
	m.mu.Unlock()

	errs := acquireCloseErrs
	for _, worker := range workers {
		if err := m.closeWorker(ctx, worker); err != nil {
			m.logEvent("minimal_link_release_failed", worker, err)
			errs = append(errs, err)
			continue
		}
		m.logEvent("minimal_link_worker_closed", worker, nil)
	}
	return errors.Join(errs...)
}

func (m *minimalLinkWorkerManager) recordConfirmedRelease(threadID string, generation uint64, sessionIdentity string) {
	threadID = strings.TrimSpace(threadID)
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	if m == nil || threadID == "" || generation == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.byThread[threadID]; current != nil {
		return
	}
	if m.confirmedRelease == nil {
		m.confirmedRelease = map[string]minimalLinkConfirmedRelease{}
	}
	m.confirmedRelease[threadID] = minimalLinkConfirmedRelease{generation: generation, sessionIdentity: sessionIdentity}
}

func (m *minimalLinkWorkerManager) forgetConfirmedReleaseLocked(threadID string, generation uint64) {
	if m.confirmedRelease == nil {
		return
	}
	if confirmed := m.confirmedRelease[threadID]; confirmed.generation == generation {
		delete(m.confirmedRelease, threadID)
	}
}

func (m *minimalLinkWorkerManager) beginAcquire(registryKey, linkedID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("minimal link worker manager is shutting down")
	}
	if _, ok := m.busyKeys[registryKey]; ok {
		return fmt.Errorf("minimal link worker key %q is busy", registryKey)
	}
	if existing := m.byKey[registryKey]; existing != nil {
		return fmt.Errorf("minimal link worker key %q is already active", registryKey)
	}
	if linkedID != "" {
		if _, ok := m.busyThreads[linkedID]; ok {
			return fmt.Errorf("minimal link worker thread %q is busy", linkedID)
		}
		if existing := m.byThread[linkedID]; existing != nil {
			return fmt.Errorf("minimal link worker thread %q is already active", linkedID)
		}
	}
	m.busyKeys[registryKey] = struct{}{}
	if linkedID != "" {
		m.busyThreads[linkedID] = struct{}{}
	}
	m.acquireWG.Add(1)
	return nil
}

func (m *minimalLinkWorkerManager) finishAcquire(registryKey, linkedID string) {
	m.mu.Lock()
	m.finishAcquireLocked(registryKey, linkedID)
	m.mu.Unlock()
}

func (m *minimalLinkWorkerManager) finishAcquireLocked(registryKey, linkedID string) {
	delete(m.busyKeys, registryKey)
	if linkedID != "" {
		delete(m.busyThreads, linkedID)
	}
	m.acquireWG.Done()
}

func (m *minimalLinkWorkerManager) factorySession() continuationSession {
	if m.factory == nil {
		return nil
	}
	return m.factory()
}

func (m *minimalLinkWorkerManager) removeLocked(worker *minimalLinkWorker) {
	if worker == nil {
		return
	}
	if current := m.byKey[worker.RegistryKey]; current == worker {
		delete(m.byKey, worker.RegistryKey)
	}
	if worker.ThreadID != "" {
		if current := m.byThread[worker.ThreadID]; current == worker {
			delete(m.byThread, worker.ThreadID)
		}
	}
	if current := m.byIdentity[worker.SessionIdentity]; current == worker {
		delete(m.byIdentity, worker.SessionIdentity)
	}
}

func (m *minimalLinkWorkerManager) closeWorker(ctx context.Context, worker *minimalLinkWorker) error {
	if worker == nil {
		return nil
	}
	if worker.cancel != nil {
		worker.cancel()
	}
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	return m.closeSession(ctx, worker.Session)
}

func (m *minimalLinkWorkerManager) closeSession(ctx context.Context, session continuationSession) error {
	if session == nil {
		return nil
	}
	closeCtx, cancel := m.contextWithCloseTimeout(ctx)
	defer cancel()
	if closer, ok := session.(contextCloseSession); ok {
		return closer.CloseContext(closeCtx)
	}
	return session.Close()
}

func (m *minimalLinkWorkerManager) closeProvisionalSession(ctx context.Context, session continuationSession) {
	m.recordAcquireCloseError(m.closeSession(ctx, session))
}

func (m *minimalLinkWorkerManager) recordAcquireCloseError(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		m.acquireCloseErrs = append(m.acquireCloseErrs, err)
	}
}

func (m *minimalLinkWorkerManager) contextWithCloseTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	timeout := m.closeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func (m *minimalLinkWorkerManager) logEvent(event string, worker *minimalLinkWorker, cause error) {
	if m == nil || m.log == nil || worker == nil {
		return
	}
	fields := lifecycleFields{
		"worker_generation":     worker.Generation,
		"session_identity_hash": shortTextHash(worker.SessionIdentity),
		"registry_key_hash":     shortTextHash(worker.RegistryKey),
		"linked_thread":         shortLogID(worker.ThreadID),
	}
	if cause != nil {
		fields["error"] = sanitizeDiagnosticString(cause.Error())
	}
	m.log(event, fields)
}

func (s *Service) acquireMinimalLinkedWorker(ctx context.Context, registryKey, linkedID string) (*minimalLinkWorker, error) {
	if s == nil || s.minimalWorkers == nil {
		return nil, errors.New("minimal link worker manager is unavailable")
	}
	worker, err := s.minimalWorkers.Acquire(ctx, registryKey, linkedID)
	if err != nil {
		return nil, err
	}
	s.startMinimalLinkedWorkerEventLoop(worker)
	return worker, nil
}

func (s *Service) startMinimalLinkedWorkerEventLoop(worker *minimalLinkWorker) {
	if s == nil || worker == nil {
		return
	}
	worker.loopOnce.Do(func() {
		loopCtx := worker.Context
		if loopCtx == nil {
			loopCtx = context.Background()
		}
		s.spawn(loopCtx, func(ctx context.Context) {
			s.minimalLinkedWorkerEventLoop(ctx, worker)
		})
	})
}

func (s *Service) minimalLinkedWorkerEventLoop(ctx context.Context, worker *minimalLinkWorker) {
	if s == nil || worker == nil || worker.Events == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-worker.Events:
			if !ok {
				return
			}
			s.handleMinimalLinkedWorkerEvent(ctx, worker, event)
		}
	}
}

func (s *Service) handleMinimalLinkedWorkerEvent(ctx context.Context, worker *minimalLinkWorker, event control.Event) {
	if s == nil || worker == nil {
		return
	}
	if !s.activeMinimalLinkedWorker(worker) {
		return
	}
	if event.Channel == "transport_error" || event.Channel == "transport_closed" {
		s.logLifecycle("minimal_link_worker_event_fault", lifecycleFields{
			"worker_generation":     worker.Generation,
			"session_identity_hash": shortTextHash(worker.SessionIdentity),
			"linked_thread":         shortLogID(worker.ThreadID),
			"channel":               event.Channel,
			"param_count":           len(event.Params),
		})
		return
	}
	threadID := threadIDFromEvent(event)
	if threadID != "" {
		_ = s.store.MarkLiveEvent(ctx, threadID, model.NowString())
	}
	if s.handleMinimalApprovalServerRequest(ctx, worker.Session, worker.SessionIdentity, event) {
		return
	}
	if s.handleMinimalApprovalResolved(ctx, worker.SessionIdentity, event) {
		return
	}
	if s.handlePendingApprovalResolved(ctx, worker.SessionIdentity, event) {
		return
	}
	if s.handlePendingServerRequest(ctx, worker.SessionIdentity, event) {
		return
	}
	if threadID != "" && threadID == strings.TrimSpace(worker.ThreadID) {
		_, _ = s.refreshThread(ctx, worker.Session, threadID)
	}
}

func (s *Service) activeMinimalLinkedWorker(worker *minimalLinkWorker) bool {
	if s == nil || worker == nil || s.minimalWorkers == nil {
		return false
	}
	current, ok := s.minimalWorkers.BySessionIdentity(worker.SessionIdentity)
	if !ok || current == nil {
		return false
	}
	return current == worker &&
		current.Generation == worker.Generation &&
		strings.TrimSpace(current.ThreadID) == strings.TrimSpace(worker.ThreadID) &&
		strings.TrimSpace(current.SessionIdentity) == strings.TrimSpace(worker.SessionIdentity)
}
