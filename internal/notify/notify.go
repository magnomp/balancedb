package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// outcomesChannel is the single global LISTEN/NOTIFY channel the processor emits
// decisions on (ADR-0002). It is per-database, not schema-scoped, so a Notifier may
// receive payloads for another cell's schema sharing the database — harmless, since
// dispatch is keyed by the operation/transaction id and a waiter only registers for
// ids it just inserted. Kept literal to match the processor's pg_notify('outcomes').
const listenOutcomes = `LISTEN outcomes`

// defaultReconnectDelay is the pause between listen-connection attempts after the
// connection drops. A dropped connection loses notifications during the gap; the API
// wait path's status poll is the durability fallback for exactly that window.
const defaultReconnectDelay = time.Second

// Notifier holds one dedicated LISTEN outcomes connection for an API process and
// demultiplexes each notification payload ('op:<id>' | 'tx:<id>') to the waiters
// registered for that key. The subscription registry is in memory, so it survives a
// listen-connection reconnect unchanged (reconnect only re-issues LISTEN; ADR-0002).
// All methods are safe for concurrent use.
type Notifier struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// ReconnectDelay is the pause between listen-connection attempts after a drop.
	// Zero means defaultReconnectDelay. Set it before Run; it is read once per retry.
	ReconnectDelay time.Duration

	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
	pid     uint32
}

// New builds a Notifier over the given pool. Run must be called (typically in its own
// goroutine) to arm the listen connection and begin dispatching.
func New(pool *pgxpool.Pool, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		pool:    pool,
		log:     log,
		waiters: make(map[string]map[chan struct{}]struct{}),
	}
}

// Register subscribes a waiter to key ('op:<id>' or 'tx:<id>'). It returns a
// buffered channel that receives a single signal on each matching notification
// (coalesced: never blocks the dispatcher) and a cancel func the caller MUST call
// (typically via defer) to unregister. Register itself never touches the database:
// the caller registers first, then does its one status check, closing the
// lost-wakeup race where the decision committed before registration.
func (n *Notifier) Register(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	set := n.waiters[key]
	if set == nil {
		set = make(map[chan struct{}]struct{})
		n.waiters[key] = set
	}
	set[ch] = struct{}{}
	n.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			n.mu.Lock()
			if s := n.waiters[key]; s != nil {
				delete(s, ch)
				if len(s) == 0 {
					delete(n.waiters, key)
				}
			}
			n.mu.Unlock()
		})
	}
	return ch, cancel
}

// dispatch delivers a notification to every waiter registered for key. The send is
// non-blocking against a size-1 buffer: a waiter that has not yet drained a prior
// signal simply keeps it (coalesced), and the dispatcher never blocks on a slow or
// departed waiter.
func (n *Notifier) dispatch(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.waiters[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run holds the dedicated listen connection and dispatches notifications until ctx
// is cancelled. On any connection failure it logs, waits ReconnectDelay, and
// re-arms (reconnect-with-resubscribe: the in-memory registry is untouched, only
// LISTEN is re-issued). It returns nil on graceful ctx cancellation.
func (n *Notifier) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := n.listen(ctx)
		if ctx.Err() != nil {
			return nil
		}
		n.log.Warn("outcomes listener dropped; reconnecting", "err", err)
		n.setPID(0)
		delay := n.ReconnectDelay
		if delay <= 0 {
			delay = defaultReconnectDelay
		}
		if sleepCtx(ctx, delay) != nil {
			return nil
		}
	}
}

// listen acquires a connection, issues LISTEN outcomes, and blocks dispatching
// notifications until the connection fails or ctx is cancelled. It always releases
// the connection on return.
func (n *Notifier) listen(ctx context.Context) error {
	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, listenOutcomes); err != nil {
		return err
	}
	n.setPID(conn.Conn().PgConn().PID())
	n.log.Debug("outcomes listener armed", "pid", n.BackendPID())

	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		n.dispatch(notif.Payload)
	}
}

// BackendPID reports the Postgres backend pid of the current listen connection, or 0
// when not armed. Useful for observability (M9) and for tests that terminate the
// listen connection to exercise the reconnect / poll-fallback paths.
func (n *Notifier) BackendPID() uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pid
}

func (n *Notifier) setPID(pid uint32) {
	n.mu.Lock()
	n.pid = pid
	n.mu.Unlock()
}

// sleepCtx sleeps for d or until ctx is cancelled, returning ctx.Err() if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
