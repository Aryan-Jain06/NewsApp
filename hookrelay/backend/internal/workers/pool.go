package workers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/config"
	"github.com/aryan-jain06/hookrelay/backend/internal/queue"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/google/uuid"
)

// readBlock is how long a stream read parks before returning empty, so a
// shutdown never waits longer than this to be noticed.
const readBlock = 2 * time.Second

// Pool runs the whole delivery pipeline: one reader, N delivery workers, a
// retry scheduler and a reaper, all shut down together.
type Pool struct {
	cfg       *config.Config
	store     *repos.Store
	queue     *queue.Queue
	policy    services.RetryPolicy
	deliverer *Deliverer

	// dispatch carries claimed stream entries from the reader and the reaper to
	// the delivery workers. It is unbuffered beyond the worker count so a slow
	// pool applies backpressure to the reader instead of piling work in memory.
	dispatch chan queue.Item
}

// NewPool wires a Pool from configuration.
func NewPool(cfg *config.Config, store *repos.Store, q *queue.Queue) (*Pool, error) {
	policy, err := services.ParseRetryPolicy(cfg.RetrySchedule)
	if err != nil {
		return nil, err
	}
	breaker := services.BreakerConfig{Threshold: cfg.BreakerThreshold, Cooldown: cfg.BreakerCooldown}
	return &Pool{
		cfg:       cfg,
		store:     store,
		queue:     q,
		policy:    policy,
		deliverer: NewDeliverer(store, breaker, policy, cfg.DeliveryTimeout, cfg.DeliveryMaxAge),
		dispatch:  make(chan queue.Item, cfg.WorkerCount),
	}, nil
}

// Run blocks until ctx is cancelled, then drains in-flight attempts and returns.
func (p *Pool) Run(ctx context.Context) error {
	if err := p.queue.EnsureGroup(ctx); err != nil {
		return err
	}
	slog.Info("delivery pool starting",
		"workers", p.cfg.WorkerCount,
		"stream", p.queue.Stream(),
		"group", p.queue.Group(),
		"consumer", p.cfg.ConsumerName,
		"delivery_timeout", p.cfg.DeliveryTimeout.String(),
		"max_attempts", p.policy.MaxAttempts(),
		"retry_schedule", p.policy.String(),
		"delivery_max_age", p.cfg.DeliveryMaxAge.String(),
	)

	var wg sync.WaitGroup

	// Delivery workers. They own the dispatch channel's consumer side and are
	// the only place a stream entry is acknowledged.
	for i := range p.cfg.WorkerCount {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p.worker(ctx, n)
		}(i)
	}

	// Reader: pulls new entries and feeds the workers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(p.dispatch)
		p.read(ctx)
	}()

	// Scheduler: makes due retries visible again.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.schedule(ctx)
	}()

	// Reaper: rescues work abandoned by a crashed worker.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.reap(ctx)
	}()

	wg.Wait()
	slog.Info("delivery pool stopped cleanly")
	return nil
}

// worker consumes dispatch until it closes.
func (p *Pool) worker(ctx context.Context, n int) {
	for item := range p.dispatch {
		p.handle(ctx, item)
	}
	slog.Debug("worker exited", "worker", n)
}

// handle runs one attempt and acknowledges the stream entry.
//
// The ordering here is the crux of at-least-once delivery: XACK happens only
// after Deliver has returned nil, meaning the attempt is committed in Postgres.
// If the process dies in between, the entry stays in the consumer group's pending
// list and the reaper hands it to another worker.
func (p *Pool) handle(ctx context.Context, item queue.Item) {
	if item.DeliveryID == uuid.Nil {
		// A malformed entry can never become valid; acknowledge and drop it.
		slog.Warn("dropping malformed stream entry", "entry_id", item.EntryID)
		p.ack(ctx, item.EntryID)
		return
	}

	outcome, err := p.deliverer.Deliver(ctx, item.DeliveryID)
	if err != nil {
		// Leave the entry pending on purpose: the reaper will retry it.
		slog.Error("delivery attempt could not be recorded; leaving entry pending for reclaim",
			"delivery_id", item.DeliveryID, "entry_id", item.EntryID, "error", err)
		return
	}

	p.ack(ctx, item.EntryID)
	if outcome != OutcomeNotClaimable {
		slog.Debug("delivery handled", "delivery_id", item.DeliveryID, "outcome", string(outcome))
	}
}

// ack acknowledges a stream entry using a context that survives shutdown, so a
// recorded attempt is never re-delivered just because we were stopping.
func (p *Pool) ack(ctx context.Context, entryID string) {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.queue.Ack(ackCtx, entryID); err != nil {
		slog.Error("xack failed; entry will be reclaimed and retried", "entry_id", entryID, "error", err)
	}
}

// read pulls new entries from the stream into dispatch.
func (p *Pool) read(ctx context.Context) {
	count := int64(p.cfg.WorkerCount)
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := p.queue.Read(ctx, p.cfg.ConsumerName, count, readBlock)
		switch {
		case errors.Is(err, queue.ErrNoWork):
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			slog.Error("stream read failed", "error", err)
			sleepCtx(ctx, time.Second)
			continue
		}
		if !p.fanIn(ctx, items) {
			return
		}
	}
}

// fanIn pushes items to the workers, returning false once ctx is done.
func (p *Pool) fanIn(ctx context.Context, items []queue.Item) bool {
	for _, item := range items {
		select {
		case p.dispatch <- item:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// schedule re-enqueues deliveries whose next_attempt_at has arrived. Running on
// a short tick is what turns a next_attempt_at timestamp into an actual retry.
func (p *Pool) schedule(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.SchedulerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ids, err := p.store.Deliveries.DueForRetry(ctx, time.Now(), p.cfg.SchedulerBatchSize, services.EnqueueLease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("scheduler query failed", "error", err)
			continue
		}
		if len(ids) == 0 {
			continue
		}
		if _, err := p.queue.EnqueueMany(ctx, ids); err != nil {
			// The lease keeps these rows due, so the next tick retries them.
			slog.Error("scheduler enqueue failed; rows stay due for the next tick",
				"count", len(ids), "error", err)
			continue
		}
		slog.Debug("scheduler re-enqueued due deliveries", "count", len(ids))
	}
}

// reap recovers work that a crashed worker was holding, in two complementary
// ways on every tick.
func (p *Pool) reap(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.ReaperInterval)
	defer ticker.Stop()
	cursor := "0-0"

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// 1. Rows stuck in 'delivering' belong to a worker that died between
		//    claiming and recording. Put them back to 'pending' and re-enqueue,
		//    because their original stream entry may already have been acked.
		stale, err := p.store.Deliveries.ReclaimStale(ctx,
			time.Now().Add(-p.cfg.ReaperMinIdle), p.cfg.ReaperBatchSize, services.EnqueueLease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("reclaim stale deliveries failed", "error", err)
		} else if len(stale) > 0 {
			slog.Warn("reclaimed deliveries abandoned mid-attempt", "count", len(stale))
			if _, err := p.queue.EnqueueMany(ctx, stale); err != nil {
				slog.Error("re-enqueue reclaimed deliveries failed", "count", len(stale), "error", err)
			}
		}

		// 2. Stream entries still pending in the consumer group after
		//    ReaperMinIdle belong to a consumer that never acked. XAUTOCLAIM
		//    transfers them to this consumer so they get another attempt — this
		//    is the mechanism that makes a hard worker kill lossless.
		items, next, err := p.queue.AutoClaim(ctx, p.cfg.ConsumerName,
			p.cfg.ReaperMinIdle, int64(p.cfg.ReaperBatchSize), cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("xautoclaim failed", "error", err)
			continue
		}
		cursor = next
		if cursor == "" {
			cursor = "0-0"
		}
		if len(items) > 0 {
			slog.Warn("reclaimed pending stream entries", "count", len(items), "consumer", p.cfg.ConsumerName)
			if !p.fanIn(ctx, items) {
				return
			}
		}
	}
}

// sleepCtx sleeps unless ctx finishes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
