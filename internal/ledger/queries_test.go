package ledger

import (
	"errors"
	"math"
	"testing"
)

func TestProjectionAdditionDoesNotWrap(t *testing.T) {
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}, {math.MaxInt64, math.MaxInt64}, {math.MinInt64, math.MinInt64}} {
		if _, err := addBalance(pair[0], pair[1]); !errors.Is(err, ErrBalanceOverflow) {
			t.Fatalf("%v: %v", pair, err)
		}
	}
	for _, tc := range [][3]int64{{math.MaxInt64, -1, math.MaxInt64 - 1}, {math.MinInt64, 1, math.MinInt64 + 1}, {math.MaxInt64, math.MinInt64, -1}, {0, math.MinInt64, math.MinInt64}} {
		got, err := addBalance(tc[0], tc[1])
		if err != nil || got != tc[2] {
			t.Fatalf("%v: %d %v", tc, got, err)
		}
	}
}
