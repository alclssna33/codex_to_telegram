package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestMinimalLinkWorkerReleaseIsThreadScoped(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, 2*time.Second, nil)
	a, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(a) failed: %v", err)
	}
	b, err := manager.Acquire(context.Background(), "link:b", "linked-b")
	if err != nil {
		t.Fatalf("Acquire(b) failed: %v", err)
	}

	closed, err := manager.Release(context.Background(), "linked-a", a.Generation)
	if err != nil || !closed {
		t.Fatalf("Release(a) closed=%t err=%v", closed, err)
	}
	if _, ok := manager.ByThread("linked-a"); ok {
		t.Fatal("released worker remained registered by thread")
	}
	if got, ok := manager.ByThread("linked-b"); !ok || got.Generation != b.Generation {
		t.Fatalf("worker b changed after releasing a: got=%#v ok=%t", got, ok)
	}

	sessions := factory.Sessions()
	if got := sessions[0].CloseContextCalls(); got != 1 {
		t.Fatalf("worker a close calls = %d, want 1", got)
	}
	if got := sessions[1].CloseContextCalls(); got != 0 {
		t.Fatalf("worker b close calls = %d, want 0", got)
	}
}

func TestMinimalLinkWorkerAcquireRejectsBusyRegistryKey(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	if _, err := manager.Acquire(context.Background(), "link:a", "linked-a"); err != nil {
		t.Fatalf("initial Acquire failed: %v", err)
	}

	if _, err := manager.Acquire(context.Background(), "link:a", "linked-other"); err == nil {
		t.Fatal("Acquire succeeded for busy registry key")
	}
	if got := len(factory.Sessions()); got != 1 {
		t.Fatalf("busy registry key started %d sessions, want 1", got)
	}
}

func TestMinimalLinkWorkerReleaseRejectsStaleGeneration(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	closed, err := manager.Release(context.Background(), "linked-a", worker.Generation+1)
	if err != nil {
		t.Fatalf("stale Release returned error: %v", err)
	}
	if closed {
		t.Fatal("stale generation release closed the worker")
	}
	if got, ok := manager.ByThread("linked-a"); !ok || got.Generation != worker.Generation {
		t.Fatalf("stale generation removed worker: got=%#v ok=%t", got, ok)
	}
	if got := factory.Sessions()[0].CloseContextCalls(); got != 0 {
		t.Fatalf("stale generation close calls = %d, want 0", got)
	}
}

func TestMinimalLinkWorkerIdentityExpiresAcrossGenerations(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	first, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(first) failed: %v", err)
	}
	if got, ok := manager.BySessionIdentity(first.SessionIdentity); !ok || got.Generation != first.Generation {
		t.Fatalf("identity lookup for first worker = %#v ok=%t", got, ok)
	}
	if closed, err := manager.Release(context.Background(), "linked-a", first.Generation); err != nil || !closed {
		t.Fatalf("Release(first) closed=%t err=%v", closed, err)
	}
	second, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(second) failed: %v", err)
	}
	if first.SessionIdentity == second.SessionIdentity {
		t.Fatalf("session identity reused across generations: %q", first.SessionIdentity)
	}
	if _, ok := manager.BySessionIdentity(first.SessionIdentity); ok {
		t.Fatal("released worker identity remained registered")
	}
	if got, ok := manager.BySessionIdentity(second.SessionIdentity); !ok || got.Generation != second.Generation {
		t.Fatalf("identity lookup for second worker = %#v ok=%t", got, ok)
	}
}

func TestMinimalLinkWorkerRecordsConfirmedReleaseForExactGeneration(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	closed, err := manager.Release(context.Background(), "linked-a", worker.Generation)
	if err != nil || !closed {
		t.Fatalf("Release closed=%t err=%v, want true nil", closed, err)
	}

	if !manager.ConfirmedRelease("linked-a", worker.Generation) {
		t.Fatalf("confirmed release missing for exact generation %d", worker.Generation)
	}
	if manager.ConfirmedRelease("linked-a", worker.Generation+1) {
		t.Fatal("confirmed release authorized a newer generation")
	}
	if manager.ConfirmedRelease("linked-other", worker.Generation) {
		t.Fatal("confirmed release authorized a different thread")
	}
}

func TestMinimalLinkWorkerAcquireClearsOlderConfirmedReleaseForNewGeneration(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	first, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(first) failed: %v", err)
	}
	if closed, err := manager.Release(context.Background(), "linked-a", first.Generation); err != nil || !closed {
		t.Fatalf("Release(first) closed=%t err=%v", closed, err)
	}
	if !manager.ConfirmedRelease("linked-a", first.Generation) {
		t.Fatalf("confirmed release missing for released generation %d", first.Generation)
	}

	second, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(second) failed: %v", err)
	}

	if second.Generation == first.Generation {
		t.Fatalf("new worker reused generation %d", second.Generation)
	}
	if manager.ConfirmedRelease("linked-a", first.Generation) {
		t.Fatal("older confirmed release survived newer worker bind")
	}
	if manager.ConfirmedRelease("linked-a", second.Generation) {
		t.Fatal("new worker generation was incorrectly treated as already released")
	}
}

func TestMinimalLinkWorkerBindThreadClearsOlderConfirmedReleaseForNewGeneration(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	first, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(first) failed: %v", err)
	}
	if closed, err := manager.Release(context.Background(), "linked-a", first.Generation); err != nil || !closed {
		t.Fatalf("Release(first) closed=%t err=%v", closed, err)
	}
	if !manager.ConfirmedRelease("linked-a", first.Generation) {
		t.Fatalf("confirmed release missing for released generation %d", first.Generation)
	}
	second, err := manager.Acquire(context.Background(), "link:unbound", "")
	if err != nil {
		t.Fatalf("Acquire(unbound) failed: %v", err)
	}

	if err := manager.BindThread(second, "linked-a"); err != nil {
		t.Fatalf("BindThread failed: %v", err)
	}

	if manager.ConfirmedRelease("linked-a", first.Generation) {
		t.Fatal("older confirmed release survived newer worker BindThread")
	}
	if manager.ConfirmedRelease("linked-a", second.Generation) {
		t.Fatal("bound worker generation was incorrectly treated as already released")
	}
}

func TestMinimalLinkWorkerCloseFailureDoesNotRecordConfirmedRelease(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	factory.Single(t).SetCloseErr(errors.New("close failed"))

	closed, err := manager.Release(context.Background(), "linked-a", worker.Generation)
	if err == nil || !closed {
		t.Fatalf("Release closed=%t err=%v, want close error after matching worker", closed, err)
	}

	if manager.ConfirmedRelease("linked-a", worker.Generation) {
		t.Fatal("close failure recorded confirmed release")
	}
}

func TestMinimalLinkWorkerCloseUsesIndependentContextAfterLoopCancel(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	closed, err := manager.Release(worker.Context, "linked-a", worker.Generation)
	if err != nil || !closed {
		t.Fatalf("Release with worker context closed=%t err=%v, want true nil", closed, err)
	}

	if got := factory.Single(t).CloseContextCalls(); got != 1 {
		t.Fatalf("close context calls = %d, want 1", got)
	}
	if !manager.ConfirmedRelease("linked-a", worker.Generation) {
		t.Fatalf("confirmed release missing after closing generation %d", worker.Generation)
	}
}

func TestMinimalLinkWorkerCloseConfirmedExitRecordsRelease(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	factory.Single(t).SetCloseErr(&appserver.CommandExitError{Err: errors.Join(context.DeadlineExceeded, errors.New("signal: killed")), Confirmed: true, Forced: true})

	closed, err := manager.Release(context.Background(), "linked-a", worker.Generation)
	if err != nil || !closed {
		t.Fatalf("Release after confirmed forced exit closed=%t err=%v, want true nil", closed, err)
	}

	if !manager.ConfirmedRelease("linked-a", worker.Generation) {
		t.Fatalf("confirmed release missing after confirmed forced exit for generation %d", worker.Generation)
	}
}

func TestMinimalLinkWorkerCloseUnconfirmedDeadlineDoesNotRecordRelease(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	factory.Single(t).SetCloseErr(context.DeadlineExceeded)

	closed, err := manager.Release(context.Background(), "linked-a", worker.Generation)
	if err == nil || !closed {
		t.Fatalf("Release after unconfirmed timeout closed=%t err=%v, want close error", closed, err)
	}

	if manager.ConfirmedRelease("linked-a", worker.Generation) {
		t.Fatal("unconfirmed deadline recorded confirmed release")
	}
}

func TestMinimalLinkedWorkerEventLoopStopsOnReleaseWithoutChannelClose(t *testing.T) {
	svc, _ := newMinimalService(t)
	factory := newWorkerSessionFactory()
	svc.minimalWorkerFactory = factory.New
	svc.minimalWorkers = newMinimalLinkWorkerManager(factory.New, time.Second, svc.logLifecycle)
	worker, err := svc.acquireMinimalLinkedWorker(context.Background(), "link:linked-loop", "linked-loop")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		svc.wg.Wait()
		close(done)
	}()
	closed, err := svc.minimalWorkers.Release(context.Background(), "linked-loop", worker.Generation)
	if err != nil || !closed {
		t.Fatalf("Release closed=%t err=%v, want true nil", closed, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker event loop waited for subscriber channel close after release")
	}
}

func TestMinimalLinkedWorkerTransportFaultDoesNotMutateGlobalLiveState(t *testing.T) {
	svc, _ := newMinimalService(t)
	liveEvents := make(chan control.Event)
	svc.mu.Lock()
	svc.live = &stubSession{}
	svc.liveEvents = liveEvents
	svc.liveConnected = true
	svc.liveGeneration = 42
	svc.mu.Unlock()
	worker := &minimalLinkWorker{
		ThreadID:        "linked-worker",
		Generation:      3,
		SessionIdentity: "minimal-link-worker:3",
		Session:         &workerSession{},
	}

	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, control.Event{Channel: "transport_error", Params: map[string]any{"message": "worker failed"}})

	svc.mu.RLock()
	liveConnected, liveGeneration, liveEventsMatch := svc.liveConnected, svc.liveGeneration, svc.liveEvents == liveEvents
	svc.mu.RUnlock()
	if !liveConnected || liveGeneration != 42 || !liveEventsMatch {
		t.Fatalf("global live state changed after worker fault: connected=%t generation=%d eventsMatch=%t", liveConnected, liveGeneration, liveEventsMatch)
	}
}

func TestMinimalLinkedWorkerEventRejectsStaleReplacedWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	oldWorker, workerApp := acquireApprovalWorker(t, svc, "linked-stale-event", "turn-stale-event")
	if closed, err := svc.minimalWorkers.Release(ctx, oldWorker.ThreadID, oldWorker.Generation); err != nil || !closed {
		t.Fatalf("Release(old) closed=%t err=%v, want true nil", closed, err)
	}
	newWorker, err := svc.minimalWorkers.Acquire(ctx, "link:"+oldWorker.ThreadID, oldWorker.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if newWorker.Generation == oldWorker.Generation || newWorker.SessionIdentity == oldWorker.SessionIdentity {
		t.Fatalf("replacement worker did not advance identity: old=%d/%q new=%d/%q", oldWorker.Generation, oldWorker.SessionIdentity, newWorker.Generation, newWorker.SessionIdentity)
	}
	event := approvalEventIdentity("req-stale-worker-event", oldWorker.ThreadID, "turn-stale-event", minimalCommandApprovalKind, map[string]any{"command": "safe"})
	workerApp.setCurrentRequest(event)

	svc.handleMinimalLinkedWorkerEvent(ctx, oldWorker, event)

	requestID := model.ScopedRequestID(oldWorker.SessionIdentity, "req-stale-worker-event")
	if approval, err := svc.store.GetMinimalApproval(ctx, requestID); err != nil {
		t.Fatal(err)
	} else if approval != nil {
		t.Fatalf("stale worker event created approval: %#v", approval)
	}
	if count := countDeliveryKind(t, svc, minimalApprovalQueueKind); count != 0 {
		t.Fatalf("stale worker event created approval deliveries=%d, want none", count)
	}
	if got, ok := svc.minimalWorkers.BySessionIdentity(newWorker.SessionIdentity); !ok || got.Generation != newWorker.Generation {
		t.Fatalf("replacement worker registry=%#v ok=%t, want active generation %d", got, ok, newWorker.Generation)
	}
}

func TestMinimalLinkWorkerAcquireRollsBackFactoryStartAndSubscribeFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*workerSessionFactory)
	}{
		{
			name: "factory",
			setup: func(factory *workerSessionFactory) {
				factory.ReturnNilSessionOnce()
			},
		},
		{
			name: "start",
			setup: func(factory *workerSessionFactory) {
				factory.FailStartOnce(errors.New("start failed"))
			},
		},
		{
			name: "subscribe",
			setup: func(factory *workerSessionFactory) {
				factory.ReturnNilEventsOnce()
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			factory := newWorkerSessionFactory()
			tc.setup(factory)
			manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
			if _, err := manager.Acquire(context.Background(), "link:a", "linked-a"); err == nil {
				t.Fatalf("Acquire succeeded after %s failure", tc.name)
			}
			if _, ok := manager.ByThread("linked-a"); ok {
				t.Fatalf("%s failure left worker registered by thread", tc.name)
			}
			worker, err := manager.Acquire(context.Background(), "link:a", "linked-a")
			if err != nil {
				t.Fatalf("Acquire after %s rollback failed: %v", tc.name, err)
			}
			if got, ok := manager.ByThread("linked-a"); !ok || got.Generation != worker.Generation {
				t.Fatalf("Acquire after rollback registered worker = %#v ok=%t", got, ok)
			}
		})
	}
}

func TestMinimalLinkWorkerBindThreadKeepsExistingOwnerOnFailure(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	owner, err := manager.Acquire(context.Background(), "link:owner", "linked-a")
	if err != nil {
		t.Fatalf("Acquire(owner) failed: %v", err)
	}
	provisional, err := manager.Acquire(context.Background(), "link:provisional", "")
	if err != nil {
		t.Fatalf("Acquire(provisional) failed: %v", err)
	}

	if err := manager.BindThread(provisional, "linked-a"); err == nil {
		t.Fatal("BindThread succeeded for an already-owned thread")
	}
	if got, ok := manager.ByThread("linked-a"); !ok || got.Generation != owner.Generation {
		t.Fatalf("duplicate bind changed existing owner: got=%#v ok=%t", got, ok)
	}
	if _, ok := manager.ByThread(""); ok {
		t.Fatal("empty provisional thread key was registered")
	}
}

func TestMinimalLinkWorkerBindThreadRejectsInFlightAcquireReservation(t *testing.T) {
	factory := newWorkerSessionFactory()
	subscribeGate := factory.BlockSubscribeOnce()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	acquireDone := make(chan acquireResult, 1)
	go func() {
		worker, err := manager.Acquire(context.Background(), "link:busy", "linked-busy")
		acquireDone <- acquireResult{worker: worker, err: err}
	}()
	subscribeGate.waitEntered(t)

	provisional, err := manager.Acquire(context.Background(), "link:provisional", "")
	if err != nil {
		subscribeGate.release()
		t.Fatalf("Acquire(provisional) failed: %v", err)
	}
	bindErr := manager.BindThread(provisional, "linked-busy")

	subscribeGate.release()
	result := <-acquireDone
	if result.err != nil {
		t.Fatalf("in-flight Acquire failed after release: %v", result.err)
	}
	if result.worker == nil {
		t.Fatal("in-flight Acquire returned nil worker")
	}
	if bindErr == nil {
		t.Fatal("BindThread succeeded for a thread reserved by an in-flight Acquire")
	}
	if got, ok := manager.ByThread("linked-busy"); !ok || got.Generation != result.worker.Generation {
		t.Fatalf("reserved worker was not registered after bind rejection: got=%#v ok=%t", got, ok)
	}
	if got, ok := manager.ByThread("linked-busy"); ok && got.Generation == provisional.Generation {
		t.Fatalf("provisional worker stole reserved thread: got=%#v", got)
	}
}

func TestMinimalLinkWorkerCloseAllWaitsForInFlightAcquireAndClosesIt(t *testing.T) {
	factory := newWorkerSessionFactory()
	subscribeGate := factory.BlockSubscribeOnce()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	acquireDone := make(chan acquireResult, 1)
	go func() {
		worker, err := manager.Acquire(context.Background(), "link:busy", "linked-busy")
		acquireDone <- acquireResult{worker: worker, err: err}
	}()
	subscribeGate.waitEntered(t)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseAll(context.Background())
	}()
	waitManagerShutdownStarted(t, manager)
	select {
	case err := <-closeDone:
		t.Fatalf("CloseAll returned before in-flight Acquire cleanup finished: %v", err)
	default:
	}

	if _, err := manager.Acquire(context.Background(), "link:late", "linked-late"); err == nil {
		t.Fatal("Acquire succeeded after CloseAll shutdown began")
	}
	subscribeGate.release()
	if err := <-closeDone; err != nil {
		t.Fatalf("CloseAll failed: %v", err)
	}
	result := <-acquireDone
	if result.err == nil {
		t.Fatalf("in-flight Acquire registered during shutdown: %#v", result.worker)
	}
	if got := len(factory.Sessions()); got != 1 {
		t.Fatalf("sessions started = %d, want only the pre-shutdown in-flight session", got)
	}
	if got := factory.Sessions()[0].CloseContextCalls(); got != 1 {
		t.Fatalf("in-flight session close calls = %d, want 1", got)
	}
	if _, ok := manager.ByThread("linked-busy"); ok {
		t.Fatal("in-flight Acquire left thread registered after shutdown")
	}
}

func TestMinimalLinkWorkerCloseAllReturnsInFlightAcquireShutdownCleanupError(t *testing.T) {
	cleanupErr := errors.New("shutdown cleanup close failed")
	factory := newWorkerSessionFactory()
	subscribeGate := factory.BlockSubscribeOnce()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	acquireDone := make(chan acquireResult, 1)
	go func() {
		worker, err := manager.Acquire(context.Background(), "link:busy", "linked-busy")
		acquireDone <- acquireResult{worker: worker, err: err}
	}()
	subscribeGate.waitEntered(t)

	sessions := factory.Sessions()
	if got := len(sessions); got != 1 {
		t.Fatalf("sessions started = %d, want 1", got)
	}
	sessions[0].SetCloseErr(cleanupErr)
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseAll(context.Background())
	}()
	waitManagerShutdownStarted(t, manager)
	select {
	case err := <-closeDone:
		t.Fatalf("CloseAll returned before in-flight Acquire cleanup finished: %v", err)
	default:
	}

	subscribeGate.release()
	result := waitAcquireResult(t, acquireDone)
	if result.err == nil {
		t.Fatalf("in-flight Acquire registered during shutdown: %#v", result.worker)
	}
	if result.worker != nil {
		t.Fatalf("in-flight Acquire returned worker during shutdown: %#v", result.worker)
	}
	if err := waitCloseAllResult(t, closeDone); !errors.Is(err, cleanupErr) {
		t.Fatalf("CloseAll error = %v, want cleanup error %v", err, cleanupErr)
	}
	if got := sessions[0].CloseContextCalls(); got != 1 {
		t.Fatalf("in-flight session close calls = %d, want 1", got)
	}
	assertMinimalLinkWorkerManagerEmpty(t, manager)
	if err := manager.CloseAll(context.Background()); err != nil {
		t.Fatalf("second CloseAll repeated cleanup error: %v", err)
	}
}

func TestMinimalLinkWorkerCloseAllReturnsNilAfterSuccessfulInFlightAcquireCleanup(t *testing.T) {
	factory := newWorkerSessionFactory()
	subscribeGate := factory.BlockSubscribeOnce()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	acquireDone := make(chan acquireResult, 1)
	go func() {
		worker, err := manager.Acquire(context.Background(), "link:busy", "linked-busy")
		acquireDone <- acquireResult{worker: worker, err: err}
	}()
	subscribeGate.waitEntered(t)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseAll(context.Background())
	}()
	waitManagerShutdownStarted(t, manager)
	select {
	case err := <-closeDone:
		t.Fatalf("CloseAll returned before in-flight Acquire cleanup finished: %v", err)
	default:
	}

	subscribeGate.release()
	result := waitAcquireResult(t, acquireDone)
	if result.err == nil {
		t.Fatalf("in-flight Acquire registered during shutdown: %#v", result.worker)
	}
	if err := waitCloseAllResult(t, closeDone); err != nil {
		t.Fatalf("CloseAll returned cleanup error after successful close: %v", err)
	}
	sessions := factory.Sessions()
	if got := sessions[0].CloseContextCalls(); got != 1 {
		t.Fatalf("in-flight session close calls = %d, want 1", got)
	}
	assertMinimalLinkWorkerManagerEmpty(t, manager)
}

func TestMinimalLinkWorkerCloseAllReturnsInFlightSubscribeFailureCleanupError(t *testing.T) {
	cleanupErr := errors.New("subscribe failure close failed during shutdown")
	factory := newWorkerSessionFactory()
	subscribeGate := factory.BlockSubscribeOnce()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	acquireDone := make(chan acquireResult, 1)
	go func() {
		worker, err := manager.Acquire(context.Background(), "link:busy", "linked-busy")
		acquireDone <- acquireResult{worker: worker, err: err}
	}()
	subscribeGate.waitEntered(t)

	sessions := factory.Sessions()
	if got := len(sessions); got != 1 {
		t.Fatalf("sessions started = %d, want 1", got)
	}
	sessions[0].SetEvents(nil)
	sessions[0].SetCloseErr(cleanupErr)
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseAll(context.Background())
	}()
	waitManagerShutdownStarted(t, manager)
	select {
	case err := <-closeDone:
		t.Fatalf("CloseAll returned before subscribe failure cleanup finished: %v", err)
	default:
	}

	subscribeGate.release()
	result := waitAcquireResult(t, acquireDone)
	if result.err == nil {
		t.Fatalf("in-flight Acquire succeeded after subscribe failure: %#v", result.worker)
	}
	if result.worker != nil {
		t.Fatalf("in-flight Acquire returned worker after subscribe failure: %#v", result.worker)
	}
	if err := waitCloseAllResult(t, closeDone); !errors.Is(err, cleanupErr) {
		t.Fatalf("CloseAll error = %v, want cleanup error %v", err, cleanupErr)
	}
	if got := sessions[0].CloseContextCalls(); got != 1 {
		t.Fatalf("in-flight session close calls = %d, want 1", got)
	}
	assertMinimalLinkWorkerManagerEmpty(t, manager)
}

func TestMinimalLinkWorkerBindThreadRejectsAfterShutdown(t *testing.T) {
	factory := newWorkerSessionFactory()
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	provisional, err := manager.Acquire(context.Background(), "link:provisional", "")
	if err != nil {
		t.Fatalf("Acquire(provisional) failed: %v", err)
	}
	if err := manager.CloseAll(context.Background()); err != nil {
		t.Fatalf("CloseAll failed: %v", err)
	}

	if err := manager.BindThread(provisional, "linked-after-close"); err == nil {
		t.Fatal("BindThread succeeded after manager shutdown")
	}
}

func TestServiceCloseClosesMinimalWorkersBeforeObserverSessions(t *testing.T) {
	service := newTestService(t)
	recorder := &closeOrderRecorder{}
	factory := newWorkerSessionFactory()
	factory.SetCloseHook(func(id string) {
		recorder.Add("worker:" + id)
	})
	manager := newMinimalLinkWorkerManager(factory.New, time.Second, nil)
	if _, err := manager.Acquire(context.Background(), "link:a", "linked-a"); err != nil {
		t.Fatalf("Acquire(worker) failed: %v", err)
	}
	service.minimalWorkers = manager
	service.live = &orderedCloseSession{stubSession: stubSession{}, label: "live", recorder: recorder}
	service.poll = &orderedCloseSession{stubSession: stubSession{}, label: "poll", recorder: recorder}

	if err := service.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	order := recorder.Entries()
	workerIndex := indexOfPrefix(order, "worker:")
	liveIndex := indexOf(order, "live")
	pollIndex := indexOf(order, "poll")
	if workerIndex < 0 || liveIndex < 0 || pollIndex < 0 {
		t.Fatalf("close order missing entries: %v", order)
	}
	if !(workerIndex < liveIndex && workerIndex < pollIndex) {
		t.Fatalf("close order = %v, want worker before live and poll", order)
	}
}

type acquireResult struct {
	worker *minimalLinkWorker
	err    error
}

type workerSessionFactory struct {
	mu             sync.Mutex
	sessions       []*workerSession
	nilSessionOnce bool
	nilEventsOnce  bool
	startErrOnce   error
	closeHook      func(string)
	blockSubscribe *subscribeGate
}

func newWorkerSessionFactory() *workerSessionFactory {
	return &workerSessionFactory{}
}

func (f *workerSessionFactory) New() continuationSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nilSessionOnce {
		f.nilSessionOnce = false
		return nil
	}
	id := fmt.Sprintf("session-%d", len(f.sessions)+1)
	session := &workerSession{
		id:             id,
		events:         make(chan control.Event, 1),
		startErr:       f.startErrOnce,
		closeHook:      f.closeHook,
		subscribeBlock: f.blockSubscribe,
	}
	f.startErrOnce = nil
	f.blockSubscribe = nil
	if f.nilEventsOnce {
		f.nilEventsOnce = false
		session.events = nil
	}
	f.sessions = append(f.sessions, session)
	return session
}

func (f *workerSessionFactory) Sessions() []*workerSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*workerSession, len(f.sessions))
	copy(out, f.sessions)
	return out
}

func (f *workerSessionFactory) Single(t *testing.T) *workerSession {
	t.Helper()
	sessions := f.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("worker sessions=%d, want 1", len(sessions))
	}
	return sessions[0]
}

func (f *workerSessionFactory) ReturnNilSessionOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nilSessionOnce = true
}

func (f *workerSessionFactory) ReturnNilEventsOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nilEventsOnce = true
}

func (f *workerSessionFactory) FailStartOnce(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErrOnce = err
}

func (f *workerSessionFactory) SetCloseHook(hook func(string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeHook = hook
}

func (f *workerSessionFactory) BlockSubscribeOnce() *subscribeGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := newSubscribeGate()
	f.blockSubscribe = gate
	return gate
}

type subscribeGate struct {
	entered     chan struct{}
	releaseCh   chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newSubscribeGate() *subscribeGate {
	return &subscribeGate{
		entered:   make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
}

func (g *subscribeGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(time.Second):
		t.Fatal("blocked Subscribe was not entered")
	}
}

func (g *subscribeGate) release() {
	g.releaseOnce.Do(func() {
		close(g.releaseCh)
	})
}

type workerSession struct {
	stubSession
	mu                sync.Mutex
	id                string
	events            chan control.Event
	startErr          error
	closeErr          error
	closeHook         func(string)
	subscribeBlock    *subscribeGate
	confirmedOnDone   bool
	startCalls        int
	subscribeCalls    int
	closeCalls        int
	closeContextCalls int
}

func (s *workerSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	return s.startErr
}

func (s *workerSession) Subscribe() <-chan control.Event {
	s.mu.Lock()
	block := s.subscribeBlock
	s.subscribeCalls++
	s.mu.Unlock()
	if block != nil {
		block.enteredOnce.Do(func() {
			close(block.entered)
		})
		<-block.releaseCh
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

func (s *workerSession) Close() error {
	s.mu.Lock()
	s.closeCalls++
	closeErr := s.closeErr
	id := s.id
	hook := s.closeHook
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return closeErr
}

func (s *workerSession) CloseContext(ctx context.Context) error {
	s.mu.Lock()
	s.closeContextCalls++
	closeErr := s.closeErr
	id := s.id
	hook := s.closeHook
	confirmedOnDone := s.confirmedOnDone
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if confirmedOnDone {
		<-ctx.Done()
		return &appserver.CommandExitError{Err: ctx.Err(), Confirmed: true, Forced: true}
	}
	if closeErr != nil {
		return closeErr
	}
	return ctx.Err()
}

func (s *workerSession) CloseContextCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeContextCalls
}

func (s *workerSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls > 0 || s.closeContextCalls > 0
}

func (s *workerSession) SetCloseErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeErr = err
}

func (s *workerSession) SetConfirmedCloseOnContextDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmedOnDone = true
}

func (s *workerSession) SetEvents(events chan control.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = events
}

func (s *workerSession) ThreadFork(context.Context, string, control.ThreadForkOptions) (map[string]any, error) {
	return map[string]any{}, nil
}

func (s *workerSession) ThreadSetName(context.Context, string, string) (map[string]any, error) {
	return map[string]any{}, nil
}

type orderedCloseSession struct {
	stubSession
	label    string
	recorder *closeOrderRecorder
}

func (s *orderedCloseSession) Close() error {
	s.recorder.Add(s.label)
	return nil
}

type closeOrderRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *closeOrderRecorder) Add(entry string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
}

func (r *closeOrderRecorder) Entries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.entries))
	copy(out, r.entries)
	return out
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func indexOfPrefix(values []string, prefix string) int {
	for i, value := range values {
		if strings.HasPrefix(value, prefix) {
			return i
		}
	}
	return -1
}

func waitAcquireResult(t *testing.T, acquireDone <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case result := <-acquireDone:
		return result
	case <-time.After(time.Second):
		t.Fatal("Acquire did not finish")
		return acquireResult{}
	}
}

func waitCloseAllResult(t *testing.T, closeDone <-chan error) error {
	t.Helper()
	select {
	case err := <-closeDone:
		return err
	case <-time.After(time.Second):
		t.Fatal("CloseAll did not finish")
		return nil
	}
}

func assertMinimalLinkWorkerManagerEmpty(t *testing.T, manager *minimalLinkWorkerManager) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.byKey) != 0 || len(manager.byThread) != 0 || len(manager.byIdentity) != 0 || len(manager.busyKeys) != 0 || len(manager.busyThreads) != 0 {
		t.Fatalf("manager retained workers/reservations: byKey=%d byThread=%d byIdentity=%d busyKeys=%d busyThreads=%d",
			len(manager.byKey), len(manager.byThread), len(manager.byIdentity), len(manager.busyKeys), len(manager.busyThreads))
	}
}

func waitManagerShutdownStarted(t *testing.T, manager *minimalLinkWorkerManager) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("manager shutdown did not start")
		case <-ticker.C:
		}
	}
}
