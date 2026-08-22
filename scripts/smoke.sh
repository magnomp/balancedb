#!/usr/bin/env bash
# scripts/smoke.sh — BalanceDB end-to-end smoke test (plan §M11 "Done when").
#
# Brings up the prod-like docker-compose stack from a clean checkout and proves:
#   1. a group insert with a synchronous wait is CONFIRMED, and balances read back;
#   2. killing the active processor container fails over to the standby within the
#      lease TTL, after which work resumes (a fresh insert is CONFIRMED by the new
#      leader).
#
# Self-contained: builds the image, runs the checks, and tears the stack down on
# exit. Set KEEP_UP=1 to leave the stack running for inspection. Exits non-zero on
# the first failure. M13 (CI) reuses this target verbatim.
set -euo pipefail

# Run from the repository root regardless of the caller's cwd.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

API="http://127.0.0.1:8080"              # ledger endpoints (Huma, spec §10)
API_READY="http://127.0.0.1:9090/readyz" # health surface is on the metrics port (:9090)
PROC_LEADER_URL="http://127.0.0.1:9091/readyz" # service: processor
STANDBY_URL="http://127.0.0.1:9092/readyz"     # service: processor-standby
OWNER="1"
RUN="$(cat /proc/sys/kernel/random/uuid | cut -c1-8)"
ACCT_A="acct-a-${RUN}"
ACCT_B="acct-b-${RUN}"

# Failover budget: the config-table default lease_ttl_ms is 15000 (15s); allow margin.
FAILOVER_TIMEOUT="${FAILOVER_TIMEOUT:-30}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-180}" # first run builds the Go image

pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
info() { printf '\033[36m==>\033[0m %s\n' "$*"; }
fail() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2
	echo "----- recent compose logs -----" >&2
	docker compose logs --tail=40 >&2 || true
	exit 1
}

cleanup() {
	if [ "${KEEP_UP:-0}" = "1" ]; then
		info "KEEP_UP=1 — leaving the stack running (docker compose down -v to remove)."
		return
	fi
	info "Tearing down the stack."
	docker compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# uuid — one fresh idempotency key per call.
uuid() { cat /proc/sys/kernel/random/uuid; }

# curl_json METHOD PATH [BODY] [EXTRA_HEADER...] — prints "HTTP_CODE<newline>BODY".
curl_json() {
	local method="$1" url="$2" body="${3:-}"
	shift 3 || true
	local args=(-s -o /dev/stderr -w '%{http_code}' -X "$method" -H "X-Owner-Id: ${OWNER}")
	local h
	for h in "$@"; do args+=(-H "$h"); done
	if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
	curl "${args[@]}" "$url"
}

wait_for_http_ok() {
	local url="$1" timeout="$2" waited=0
	while ! curl -fs -o /dev/null "$url" 2>/dev/null; do
		sleep 2
		waited=$((waited + 2))
		if [ "$waited" -ge "$timeout" ]; then return 1; fi
	done
	return 0
}

# Returns the URL of whichever processor currently reports leader:true (empty if none).
current_leader_url() {
	local u
	for u in "$PROC_LEADER_URL" "$STANDBY_URL"; do
		if curl -fs "$u" 2>/dev/null | grep -q '"leader":true'; then
			echo "$u"
			return 0
		fi
	done
	echo ""
}

# ---------------------------------------------------------------------------
info "Building images and starting the stack (docker compose up -d --build)."
docker compose up -d --build

info "Waiting for the API to become ready (up to ${BOOT_TIMEOUT}s)."
wait_for_http_ok "$API_READY" "$BOOT_TIMEOUT" || fail "API never became ready"
pass "API is ready (${API_READY})."

info "Waiting for a processor to win the leader lease."
leader_url=""
waited=0
while :; do
	leader_url="$(current_leader_url)"
	[ -n "$leader_url" ] && break
	sleep 1
	waited=$((waited + 1))
	[ "$waited" -ge 30 ] && fail "no processor claimed leadership within 30s"
done
pass "A processor holds the leader lease (${leader_url})."

# ---------------------------------------------------------------------------
info "Inserting a 2-leg group (transfer ${ACCT_A} -100 -> ${ACCT_B} +100) with a synchronous wait."
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
group_body="$(printf '{"operations":[{"account":"%s","amount":-100,"effective_at":"%s"},{"account":"%s","amount":100,"effective_at":"%s"}],"wait_ms":10000}' \
	"$ACCT_A" "$now" "$ACCT_B" "$now")"

resp="$(curl_json POST "${API}/transactions" "$group_body" "Idempotency-Key: $(uuid)" 2>/tmp/smoke_body)"
body="$(cat /tmp/smoke_body)"
[ "$resp" = "200" ] || fail "group insert returned HTTP ${resp} (expected 200 — synchronous decision); body: ${body}"
echo "$body" | grep -q '"transaction_status":"COMMITTED"' || fail "group not COMMITTED; body: ${body}"
confirmed="$(echo "$body" | grep -o '"status":"CONFIRMED"' | wc -l | tr -d ' ')"
[ "$confirmed" = "2" ] || fail "expected 2 CONFIRMED legs, got ${confirmed}; body: ${body}"
pass "Group COMMITTED synchronously with 2 CONFIRMED legs (HTTP 200)."

# ---------------------------------------------------------------------------
info "Reading balances."
check_balance() {
	local acct="$1" want="$2" b got
	b="$(curl_json GET "${API}/accounts/${acct}/balance" "" 2>/tmp/smoke_body)"
	[ "$b" = "200" ] || fail "balance ${acct} returned HTTP ${b}"
	got="$(grep -o '"balance":-\?[0-9]\+' /tmp/smoke_body | head -1 | grep -o '\-\?[0-9]\+')"
	[ "$got" = "$want" ] || fail "balance ${acct}: got ${got}, want ${want}"
	pass "balance ${acct} = ${got}"
}
check_balance "$ACCT_A" "-100"
check_balance "$ACCT_B" "100"

# ---------------------------------------------------------------------------
info "Failover: killing the active processor; a standby must take over within the lease TTL."
if [ "$leader_url" = "$PROC_LEADER_URL" ]; then
	leader_svc="processor"
	survivor_url="$STANDBY_URL"
else
	leader_svc="processor-standby"
	survivor_url="$PROC_LEADER_URL"
fi
leader_cid="$(docker compose ps -q "$leader_svc")"
[ -n "$leader_cid" ] || fail "could not resolve container id for ${leader_svc}"

# Pin the killed container down (compose uses restart: unless-stopped for prod
# realism; we disable restart on THIS container so the failover target is
# deterministic — the survivor is the only remaining candidate).
docker update --restart=no "$leader_cid" >/dev/null
info "Killing leader service '${leader_svc}' (container ${leader_cid:0:12}) with SIGKILL — no graceful lease release."
docker kill "$leader_cid" >/dev/null

start="$(date +%s)"
while :; do
	if curl -fs "$survivor_url" 2>/dev/null | grep -q '"leader":true'; then
		elapsed="$(( $(date +%s) - start ))"
		pass "standby took over leadership in ~${elapsed}s (within ${FAILOVER_TIMEOUT}s budget)."
		break
	fi
	elapsed="$(( $(date +%s) - start ))"
	[ "$elapsed" -ge "$FAILOVER_TIMEOUT" ] && fail "standby did not become leader within ${FAILOVER_TIMEOUT}s"
	sleep 1
done

# ---------------------------------------------------------------------------
info "Proving the new leader drains work: a fresh insert must be CONFIRMED."
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
single_body="$(printf '{"operations":[{"account":"%s","amount":50,"effective_at":"%s"}],"wait_ms":10000}' "$ACCT_B" "$now")"
resp="$(curl_json POST "${API}/transactions" "$single_body" "Idempotency-Key: $(uuid)" 2>/tmp/smoke_body)"
body="$(cat /tmp/smoke_body)"
[ "$resp" = "200" ] || fail "post-failover insert returned HTTP ${resp}; body: ${body}"
echo "$body" | grep -q '"status":"CONFIRMED"' || fail "post-failover op not CONFIRMED; body: ${body}"
pass "post-failover insert CONFIRMED by the new leader."
check_balance "$ACCT_B" "150"

echo
info "SMOKE TEST PASSED."
