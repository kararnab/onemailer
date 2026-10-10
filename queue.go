package onemailer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Queue delivers messages outside the request that produced them
// (so a slow server never stalls a request): a bounded in-process queue, one
// worker, and retries on a fixed schedule. A message that cannot be queued
// or delivered is dropped and logged; the user can ask again. A permanent
// failure (see IsPermanent) is not retried.
//
// The queue lives in memory: what is queued or waiting for a retry is lost
// if the process dies, and dropped (and logged) when Close gives up. When a
// message must survive a restart, call a Sender from your own job system
// instead (README, "Delivery guarantees").
type Queue struct {
	sender   Sender
	log      *slog.Logger
	delays   []time.Duration
	after    func(time.Duration) <-chan time.Time
	onResult func(Result)

	items chan item
	stop  chan struct{} // closed when the queue gives up on what is left
	ctx   context.Context
	abort context.CancelFunc

	mu      sync.Mutex
	closed  bool
	drained chan struct{}  // closed when Close has finished
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
	// OnResult, when set, is called with every outcome the queue logs, for
	// metrics. It runs on the queue's goroutines and must return quickly.
	OnResult func(Result)
}

// Outcome is what happened to one try of a message.
type Outcome string

const (
	OutcomeSent    Outcome = "sent"     // accepted by the server
	OutcomeRetry   Outcome = "retry"    // failed; will be tried again
	OutcomeNotSent Outcome = "not_sent" // dropped for good; see Result.Reason
)

// Reasons a message is not sent (Result.Reason, and "reason" in the logs).
const (
	ReasonQueueFull  = "queue_full"  // Enqueue found the queue full
	ReasonShutdown   = "shutdown"    // the queue closed before delivery
	ReasonSMTPFailed = "smtp_failed" // every try failed
	ReasonRejected   = "rejected"    // a permanent failure; not retried
	ReasonMailOff    = "mail_off"    // the sender is off (NewSender with provider none)
)

// Result reports one outcome to QueueConfig.OnResult. Like the logs, it
// carries the message kind and attributes, never the body or recipient.
type Result struct {
	Kind     string
	Attrs    []slog.Attr
	Outcome  Outcome
	Reason   string        // with OutcomeNotSent
	Attempts int           // tries made so far
	Err      error         // the last failure, if any
	Duration time.Duration // of the last try; 0 when none was made
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
		sender: sender, log: cfg.Logger, delays: cfg.Delays, after: cfg.After, onResult: cfg.OnResult,
		items: make(chan item, cfg.Capacity), stop: make(chan struct{}), ctx: ctx, abort: abort,
		drained: make(chan struct{}),
	}
	q.worker.Add(1)
	go q.run()
	return q
}

// Enqueue accepts m for delivery and reports whether it was accepted. It
// never blocks: a full or closed queue drops m and logs why. A message
// without a Kind is logged as "mail".
func (q *Queue) Enqueue(m Message) bool {
	if m.Kind == "" {
		m.Kind = "mail"
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.drop(m, ReasonShutdown, 0, nil, 0)
		return false
	}
	q.wg.Add(1)
	select {
	case q.items <- item{m: m, queued: time.Now()}:
		return true
	default:
		q.wg.Done()
		q.drop(m, ReasonQueueFull, 0, nil, 0)
		return false
	}
}

// Len is the number of messages waiting in the queue, not counting the one
// being sent or those waiting for a retry.
func (q *Queue) Len() int { return len(q.items) }

// Close stops accepting messages and waits until every accepted one is
// sent or has failed for good, or until ctx ends; then what is left
// (queued, or waiting for a retry) is dropped and logged. A second Close
// waits for the first to finish, or for its own ctx.
func (q *Queue) Close(ctx context.Context) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		select {
		case <-q.drained:
		case <-ctx.Done():
		}
		return
	}
	q.closed = true
	q.mu.Unlock()
	defer close(q.drained)

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
			q.drop(it.m, ReasonShutdown, it.attempt, nil, 0)
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
	took := time.Since(start)
	next := it.attempt + 1
	if err == nil {
		attrs := append([]any{
			"event", it.m.Kind + "_sent", "attempts", next,
			"duration_ms", took.Milliseconds(),
		}, attrsOf(it.m)...)
		q.log.Info("mail sent", attrs...)
		q.report(Result{Kind: it.m.Kind, Attrs: it.m.Attrs, Outcome: OutcomeSent, Attempts: next, Duration: took})
		q.wg.Done()
		return
	}
	switch {
	case errors.Is(err, ErrMailOff):
		q.drop(it.m, ReasonMailOff, next, nil, took)
		q.wg.Done()
		return
	case IsPermanent(err):
		q.drop(it.m, ReasonRejected, next, err, took)
		q.wg.Done()
		return
	case next >= len(q.delays):
		q.drop(it.m, ReasonSMTPFailed, next, err, took)
		q.wg.Done()
		return
	}
	q.log.Warn("mail delivery failed; will retry", append([]any{
		"event", it.m.Kind + "_retry", "attempt", next, "smtp_code", SMTPCode(err), "error", err.Error(),
		"retry_in", q.delays[next].String(),
	}, attrsOf(it.m)...)...)
	q.report(Result{Kind: it.m.Kind, Attrs: it.m.Attrs, Outcome: OutcomeRetry, Attempts: next, Err: err, Duration: took})
	wait := q.after(q.delays[next])
	q.waiting.Add(1)
	go func() {
		defer q.waiting.Done()
		select {
		case <-wait:
			select {
			case q.items <- item{m: it.m, attempt: next, queued: it.queued}:
			case <-q.stop:
				q.drop(it.m, ReasonShutdown, next, nil, 0)
				q.wg.Done()
			}
		case <-q.stop:
			q.drop(it.m, ReasonShutdown, next, nil, 0)
			q.wg.Done()
		}
	}()
}

// drop logs a message that will not be delivered: never its body or
// recipient address, only its kind and attributes.
func (q *Queue) drop(m Message, reason string, attempts int, err error, took time.Duration) {
	attrs := []any{"event", m.Kind + "_not_sent", "reason", reason, "attempts", attempts}
	if err != nil {
		attrs = append(attrs, "smtp_code", SMTPCode(err), "error", err.Error())
	}
	level := slog.LevelWarn
	if reason == ReasonMailOff {
		level = slog.LevelInfo // configured, not a failure
	}
	q.log.Log(context.Background(), level, "mail not sent", append(attrs, attrsOf(m)...)...)
	q.report(Result{Kind: m.Kind, Attrs: m.Attrs, Outcome: OutcomeNotSent, Reason: reason, Attempts: attempts, Err: err, Duration: took})
}

func (q *Queue) report(r Result) {
	if q.onResult != nil {
		q.onResult(r)
	}
}

func attrsOf(m Message) []any {
	out := make([]any, len(m.Attrs))
	for i, a := range m.Attrs {
		out[i] = a
	}
	return out
}
