package onemailer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSender fails its first `fail` sends, then records messages.
type fakeSender struct {
	mu    sync.Mutex
	fail  int
	err   error
	sent  []Message
	tries int
	block chan struct{} // when set, Send waits on it (or ctx)
}

func (f *fakeSender) Send(ctx context.Context, m Message) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.fail > 0 {
		f.fail--
		if f.err != nil {
			return f.err
		}
		return &textproto.Error{Code: 451, Msg: "try again"}
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) count() (sent, tries int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent), f.tries
}

// clock hands out timers the test fires by hand, recording each delay.
type clock struct {
	mu     sync.Mutex
	delays []time.Duration
	timers []chan time.Time
}

func (c *clock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.delays = append(c.delays, d)
	c.timers = append(c.timers, ch)
	return ch
}

// fire fires timer i once it exists.
func (c *clock) fire(t *testing.T, i int) {
	t.Helper()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.timers) > i })
	c.mu.Lock()
	c.timers[i] <- time.Now()
	c.mu.Unlock()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newTestQueue(s Sender, capacity int) (*Queue, *clock, *logBuf) {
	c := &clock{}
	logs := &logBuf{}
	q := NewQueue(s, QueueConfig{Capacity: capacity, After: c.after, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	return q, c, logs
}

var resetMsg = Message{Kind: "password_reset", To: "ada@example.com", Subject: "s", Text: "secret-link-body",
	Attrs: []slog.Attr{slog.String("token_id", "tok-1")}}

func TestQueue_SendsAndLogsWithoutBody(t *testing.T) {
	s := &fakeSender{}
	q, _, logs := newTestQueue(s, 4)
	if !q.Enqueue(resetMsg) {
		t.Fatal("not accepted")
	}
	q.Close(context.Background())
	if sent, _ := s.count(); sent != 1 {
		t.Fatalf("sent %d", sent)
	}
	out := logs.String()
	if !strings.Contains(out, "event=password_reset_sent") || !strings.Contains(out, "attempts=1") || !strings.Contains(out, "token_id=tok-1") {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "secret-link-body") || strings.Contains(out, "ada@example.com") {
		t.Fatalf("the log holds the body or the address: %s", out)
	}
}

// AC-2: the retry schedule 0, 10 s, 1 min, 5 min; success on the third try.
func TestQueue_RetriesOnSchedule(t *testing.T) {
	s := &fakeSender{fail: 2}
	q, c, logs := newTestQueue(s, 4)
	q.Enqueue(resetMsg)
	c.fire(t, 0)
	c.fire(t, 1)
	waitFor(t, func() bool { sent, _ := s.count(); return sent == 1 })
	q.Close(context.Background())
	if _, tries := s.count(); tries != 3 {
		t.Fatalf("tries %d", tries)
	}
	if len(c.delays) != 2 || c.delays[0] != 10*time.Second || c.delays[1] != time.Minute {
		t.Fatalf("delays %v", c.delays)
	}
	if out := logs.String(); !strings.Contains(out, "event=password_reset_retry") || !strings.Contains(out, "smtp_code=451") ||
		!strings.Contains(out, "attempts=3") {
		t.Fatalf("log: %s", out)
	}
}

func TestQueue_GivesUpAfterFourTries(t *testing.T) {
	s := &fakeSender{fail: 10}
	q, c, logs := newTestQueue(s, 4)
	q.Enqueue(resetMsg)
	for i := 0; i < 3; i++ {
		c.fire(t, i)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "reason=smtp_failed") })
	q.Close(context.Background())
	if _, tries := s.count(); tries != 4 {
		t.Fatalf("tries %d", tries)
	}
	if want := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}; len(c.delays) != 3 || c.delays[2] != want[2] {
		t.Fatalf("delays %v", c.delays)
	}
	if out := logs.String(); !strings.Contains(out, "event=password_reset_not_sent") || !strings.Contains(out, "attempts=4") {
		t.Fatalf("log: %s", out)
	}
}

// AC-2: a full queue drops and logs; Enqueue never blocks.
func TestQueue_FullDrops(t *testing.T) {
	s := &fakeSender{block: make(chan struct{})}
	q, _, logs := newTestQueue(s, 1)
	q.Enqueue(resetMsg) // taken by the worker, which blocks
	waitFor(t, func() bool { return len(q.items) == 0 })
	if !q.Enqueue(resetMsg) { // fills the one slot
		t.Fatal("second not accepted")
	}
	start := time.Now()
	if q.Enqueue(resetMsg) {
		t.Fatal("third accepted by a full queue")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Enqueue blocked")
	}
	if !strings.Contains(logs.String(), "reason=queue_full") {
		t.Fatalf("log: %s", logs.String())
	}
	close(s.block)
	q.Close(context.Background())
	if sent, _ := s.count(); sent != 2 {
		t.Fatalf("sent %d", sent)
	}
}

// Close drains, then drops what waits for a retry when its context ends.
func TestQueue_CloseDropsPendingRetries(t *testing.T) {
	s := &fakeSender{fail: 10, err: errors.New("down")}
	q, c, logs := newTestQueue(s, 4)
	q.Enqueue(resetMsg)
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.timers) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	q.Close(ctx)
	if !strings.Contains(logs.String(), "reason=shutdown") {
		t.Fatalf("log: %s", logs.String())
	}
	if q.Enqueue(resetMsg) {
		t.Fatal("accepted after Close")
	}
}
