package onemailer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Queue delivers messages outside the request that produced them
// (so a slow server never stalls a request): a bounded in-process queue, one
// worker, and retries on a fixed schedule. A message that cannot be queued
// or delivered is dropped and logged; the user can ask again.
type Queue struct {
	sender Sender
	log    *slog.Logger
	delays []time.Duration
	after  func(time.Duration) <-chan time.Time

	items chan item
	stop  chan struct{} // closed when the queue gives up on what is left
	ctx   context.Context
	abort context.CancelFunc

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup // one per accepted message until sent or dropped
	worker  sync.WaitGroup
	waiting sync.WaitGroup // goroutines holding a message until its retry
}

type item struct {
	m       Message
	attempt int
	queued  time.Time
}

// QueueConfig tunes a Queue; the zero value gives the defaults.
type QueueConfig struct {
	Capacity int             // default 256
	Delays   []time.Duration // before each try; default 0, 10 s, 1 min, 5 min
	Logger   *slog.Logger    // default slog.Default()
	// After replaces time.After in tests.
	After func(time.Duration) <-chan time.Time
}

// DefaultDelays is the retry schedule: at once, then after 10 s,
// 1 min and 5 min.
var DefaultDelays = []time.Duration{0, 10 * time.Second, time.Minute, 5 * time.Minute}

// NewQueue starts a queue in front of sender.
func NewQueue(sender Sender, cfg QueueConfig) *Queue {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 256
	}
	if len(cfg.Delays) == 0 {
		cfg.Delays = DefaultDelays
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	ctx, abort := context.WithCancel(context.Background())
	q := &Queue{
		sender: sender, log: cfg.Logger, delays: cfg.Delays, after: cfg.After,
		items: make(chan item, cfg.Capacity), stop: make(chan struct{}), ctx: ctx, abort: abort,
	}
	q.worker.Add(1)
	go q.run()
	return q
}

// Enqueue accepts m for delivery and reports whether it was accepted. It
// never blocks: a full or closed queue drops m and logs why.
func (q *Queue) Enqueue(m Message) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.drop(m, "shutdown", 0, nil)
		return false
	}
	q.wg.Add(1)
	select {
	case q.items <- item{m: m, queued: time.Now()}:
		return true
	default:
		q.wg.Done()
		q.drop(m, "queue_full", 0, nil)
		return false
	}
}

// Close stops accepting messages and waits until every accepted one is
// sent or has failed for good, or until ctx ends; then what is left
// (queued, or waiting for a retry) is dropped and logged.
func (q *Queue) Close(ctx context.Context) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	close(q.stop)
	q.abort()
	q.worker.Wait()
	q.waiting.Wait()
	for {
		select {
		case it := <-q.items:
			q.drop(it.m, "shutdown", it.attempt, nil)
			q.wg.Done()
		default:
			return
		}
	}
}

func (q *Queue) run() {
	defer q.worker.Done()
	for {
		select {
		case <-q.stop:
			return
		case it := <-q.items:
			q.try(it)
		}
	}
}

func (q *Queue) try(it item) {
	start := time.Now()
	err := q.sender.Send(q.ctx, it.m)
	if err == nil {
		attrs := append([]any{
			"event", it.m.Kind + "_sent", "attempts", it.attempt + 1,
			"duration_ms", time.Since(start).Milliseconds(),
		}, attrsOf(it.m)...)
		q.log.Info("mail sent", attrs...)
		q.wg.Done()
		return
	}
	next := it.attempt + 1
	if next >= len(q.delays) {
		q.drop(it.m, "smtp_failed", next, err)
		q.wg.Done()
		return
	}
	q.log.Warn("mail delivery failed; will retry", append([]any{
		"event", it.m.Kind + "_retry", "attempt", next, "smtp_code", SMTPCode(err), "error", err.Error(),
		"retry_in", q.delays[next].String(),
	}, attrsOf(it.m)...)...)
	wait := q.after(q.delays[next])
	q.waiting.Add(1)
	go func() {
		defer q.waiting.Done()
		select {
		case <-wait:
			select {
			case q.items <- item{m: it.m, attempt: next, queued: it.queued}:
			case <-q.stop:
				q.drop(it.m, "shutdown", next, nil)
				q.wg.Done()
			}
		case <-q.stop:
			q.drop(it.m, "shutdown", next, nil)
			q.wg.Done()
		}
	}()
}

// drop logs a message that will not be delivered: never its body or
// recipient address, only its kind and attributes.
func (q *Queue) drop(m Message, reason string, attempts int, err error) {
	attrs := []any{"event", m.Kind + "_not_sent", "reason", reason, "attempts", attempts}
	if err != nil {
		attrs = append(attrs, "smtp_code", SMTPCode(err), "error", err.Error())
	}
	q.log.Warn("mail not sent", append(attrs, attrsOf(m)...)...)
}

func attrsOf(m Message) []any {
	out := make([]any, len(m.Attrs))
	for i, a := range m.Attrs {
		out[i] = a
	}
	return out
}
