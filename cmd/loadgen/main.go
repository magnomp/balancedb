// Command loadgen is a small load generator for local observability work (plan
// §M9). It POSTs operations to a running BalanceDB api node at a configurable
// rate so you can watch loop utilization rho, queue depth, doorbell wakeup lag,
// and batching behavior on the processor's :9090/metrics endpoint while it runs.
//
// It is a developer tool, not part of any deployed binary. Amounts are small and
// positive against unbounded (default) accounts, so operations are always accepted
// — the point is throughput and timing, not validation.
package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		url       = flag.String("url", "http://localhost:8080", "base URL of the api node")
		rate      = flag.Int("rate", 50, "target inserts per second")
		duration  = flag.Duration("duration", 30*time.Second, "how long to run (0 = until Ctrl-C)")
		owner     = flag.String("owner", "1", "X-Owner-Id header value")
		accounts  = flag.Int("accounts", 20, "number of distinct accounts to spread load over")
		groupSize = flag.Int("group-size", 1, "operations per request (1 = single; >1 = atomic group)")
		maxConc   = flag.Int("concurrency", 64, "maximum in-flight requests")
	)
	flag.Parse()

	if *rate < 1 {
		log.Fatal("loadgen: -rate must be >= 1")
	}
	if *accounts < 1 {
		log.Fatal("loadgen: -accounts must be >= 1")
	}
	if *groupSize < 1 {
		log.Fatal("loadgen: -group-size must be >= 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	client := &http.Client{Timeout: 10 * time.Second}
	endpoint := *url + "/transactions"

	var sent, ok, failed atomic.Int64
	sem := make(chan struct{}, *maxConc)
	var wg sync.WaitGroup

	fmt.Fprintf(os.Stderr, "loadgen: %d req/s to %s (owner=%s, accounts=%d, group-size=%d)\n",
		*rate, endpoint, *owner, *accounts, *groupSize)

	// Progress line once per second.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var prev int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s := sent.Load()
				fmt.Fprintf(os.Stderr, "sent=%d ok=%d failed=%d (%d/s)\n",
					s, ok.Load(), failed.Load(), s-prev)
				prev = s
			}
		}
	}()

	interval := time.Second / time.Duration(*rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			body := buildBody(rng, *accounts, *groupSize)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break loop
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				sent.Add(1)
				if postInsert(ctx, client, endpoint, *owner, body) {
					ok.Add(1)
				} else {
					failed.Add(1)
				}
			}()
		}
	}

	wg.Wait()
	fmt.Fprintf(os.Stderr, "loadgen: done. sent=%d ok=%d failed=%d\n", sent.Load(), ok.Load(), failed.Load())
}

// buildBody creates one insertion request: a single op, or a group of group-size
// legs across distinct accounts. Amounts are small positive minor units.
func buildBody(rng *rand.Rand, accounts, groupSize int) []byte {
	now := time.Now().UTC().Format(time.RFC3339)
	ops := make([]map[string]any, groupSize)
	for i := range ops {
		ops[i] = map[string]any{
			"account":      "acct-" + strconv.Itoa(rng.Intn(accounts)),
			"amount":       int64(rng.Intn(100) + 1),
			"effective_at": now,
		}
	}
	b, _ := json.Marshal(map[string]any{"operations": ops})
	return b
}

// postInsert sends one insertion and reports whether it was accepted (2xx).
func postInsert(ctx context.Context, client *http.Client, endpoint, owner string, body []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Owner-Id", owner)
	req.Header.Set("Idempotency-Key", newUUID())
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// newUUID returns a random v4 UUID string for the required Idempotency-Key header
// (each insert is a distinct unit of work). crypto/rand, no dependency.
func newUUID() string {
	var b [16]byte
	_, _ = cryptorand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
