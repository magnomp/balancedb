// Package lease implements the single-row leader lease (spec §7.1). Each
// processor instance generates a UUID at boot and runs the spec's single-UPDATE
// acquire/renew against the leader_lease row; the row updated means this instance
// leads the cycle, none updated means it is a standby. All time comparisons run
// in SQL against the database clock (now()) — the Go process clock is never
// trusted for lease decisions. There is no background goroutine: the processor's
// loop is the heartbeat, refreshing IsLeader by calling Acquire each cycle
// (keeping the design single-threaded). Graceful shutdown calls Release, clearing
// ownership; a crash is covered by TTL expiry.
package lease
