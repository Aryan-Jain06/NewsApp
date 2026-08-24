// Package queue wraps the Redis Stream that carries delivery work items.
//
// A stream plus a consumer group gives us three properties the delivery pipeline
// depends on: work is handed to exactly one consumer at a time, unacknowledged
// entries stay in the group's Pending Entries List (PEL) so a crashed worker's
// work is recoverable, and XAUTOCLAIM lets a survivor take that work over.
package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// fieldDeliveryID is the single field carried by each stream entry. The payload
// deliberately stays a pointer into Postgres rather than the event body: the
// database is the source of truth, and small entries keep the stream cheap.
const fieldDeliveryID = "delivery_id"

// Queue publishes and consumes delivery work items.
type Queue struct {
	rdb    *redis.Client
	stream string
	group  string
}

// New builds a Queue.
func New(rdb *redis.Client, stream, group string) *Queue {
	return &Queue{rdb: rdb, stream: stream, group: group}
}

// Client exposes the underlying Redis client for health checks.
func (q *Queue) Client() *redis.Client { return q.rdb }

// Stream returns the stream key.
func (q *Queue) Stream() string { return q.stream }

// Group returns the consumer-group name.
func (q *Queue) Group() string { return q.group }

// EnsureGroup creates the consumer group if it does not exist. MKSTREAM also
// creates the stream, so a fresh Redis needs no manual setup.
func (q *Queue) EnsureGroup(ctx context.Context) error {
	err := q.rdb.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create consumer group %q on %q: %w", q.group, q.stream, err)
	}
	return nil
}

// Enqueue appends one delivery ID to the stream and returns the entry ID.
func (q *Queue) Enqueue(ctx context.Context, deliveryID uuid.UUID) (string, error) {
	id, err := q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		Values: map[string]any{fieldDeliveryID: deliveryID.String()},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd delivery %s: %w", deliveryID, err)
	}
	return id, nil
}

// EnqueueMany appends several delivery IDs in one pipeline round trip.
func (q *Queue) EnqueueMany(ctx context.Context, deliveryIDs []uuid.UUID) ([]string, error) {
	if len(deliveryIDs) == 0 {
		return nil, nil
	}
	pipe := q.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(deliveryIDs))
	for i, id := range deliveryIDs {
		cmds[i] = pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: q.stream,
			Values: map[string]any{fieldDeliveryID: id.String()},
		})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("xadd %d deliveries: %w", len(deliveryIDs), err)
	}
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		id, err := c.Result()
		if err != nil {
			return nil, fmt.Errorf("read xadd result: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// Item is one claimed unit of work. EntryID must be acknowledged with Ack once
// the attempt has been durably recorded.
type Item struct {
	EntryID    string
	DeliveryID uuid.UUID
}

// ErrNoWork signals that a read timed out with nothing available.
var ErrNoWork = errors.New("no work available")

// Read blocks for up to block waiting for new entries for this consumer.
func (q *Queue) Read(ctx context.Context, consumer string, count int64, block time.Duration) ([]Item, error) {
	streams, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: consumer,
		Streams:  []string{q.stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNoWork
		}
		return nil, fmt.Errorf("xreadgroup: %w", err)
	}
	return itemsFrom(streams), nil
}

// Ack removes entries from the group's pending list. Calling it only after the
// attempt is recorded in Postgres is what makes delivery at-least-once: a crash
// before the Ack leaves the entry pending and therefore reclaimable.
func (q *Queue) Ack(ctx context.Context, entryIDs ...string) error {
	if len(entryIDs) == 0 {
		return nil
	}
	if err := q.rdb.XAck(ctx, q.stream, q.group, entryIDs...).Err(); err != nil {
		return fmt.Errorf("xack %d entries: %w", len(entryIDs), err)
	}
	return nil
}

// AutoClaim transfers entries that have been pending longer than minIdle to
// consumer. This is the crash-recovery path: whatever a dead worker was holding
// becomes visible to a live one.
func (q *Queue) AutoClaim(ctx context.Context, consumer string, minIdle time.Duration, count int64, start string) (items []Item, cursor string, err error) {
	msgs, next, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   q.stream,
		Group:    q.group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    start,
		Count:    count,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, "0-0", nil
		}
		return nil, "", fmt.Errorf("xautoclaim: %w", err)
	}
	return itemsFromMessages(msgs), next, nil
}

// Trim caps the stream's length so acknowledged history cannot grow unbounded.
func (q *Queue) Trim(ctx context.Context, maxLen int64) error {
	if err := q.rdb.XTrimMaxLenApprox(ctx, q.stream, maxLen, 0).Err(); err != nil {
		return fmt.Errorf("xtrim: %w", err)
	}
	return nil
}

// Depth reports the current number of entries in the stream.
func (q *Queue) Depth(ctx context.Context) (int64, error) {
	n, err := q.rdb.XLen(ctx, q.stream).Result()
	if err != nil {
		return 0, fmt.Errorf("xlen: %w", err)
	}
	return n, nil
}

// PendingCount reports how many entries the group has delivered but not acked.
func (q *Queue) PendingCount(ctx context.Context) (int64, error) {
	res, err := q.rdb.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("xpending: %w", err)
	}
	return res.Count, nil
}

func itemsFrom(streams []redis.XStream) []Item {
	var out []Item
	for _, s := range streams {
		out = append(out, itemsFromMessages(s.Messages)...)
	}
	return out
}

func itemsFromMessages(msgs []redis.XMessage) []Item {
	out := make([]Item, 0, len(msgs))
	for _, m := range msgs {
		raw, _ := m.Values[fieldDeliveryID].(string)
		id, err := uuid.Parse(raw)
		if err != nil {
			// A malformed entry can never become valid; surface it with a nil
			// delivery ID so the worker acks and drops it instead of looping.
			out = append(out, Item{EntryID: m.ID, DeliveryID: uuid.Nil})
			continue
		}
		out = append(out, Item{EntryID: m.ID, DeliveryID: id})
	}
	return out
}

// Connect opens a Redis client from a redis:// URL and pings it, retrying while
// the server comes up.
func Connect(ctx context.Context, rawURL string) (*redis.Client, error) {
	opt, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(opt)

	deadline := time.Now().Add(60 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = rdb.Ping(pingCtx).Err()
		cancel()
		if err == nil {
			return rdb, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = rdb.Close()
			return nil, fmt.Errorf("ping redis: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = rdb.Close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
