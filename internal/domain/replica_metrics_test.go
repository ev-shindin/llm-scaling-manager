package domain

import "testing"

// A record of one rank is its own busiest rank, whatever the busiest-rank
// fields hold: the guards read exactly what they read before ranks existed.
func TestBusiestRankOfOneRankIsTheRecord(t *testing.T) {
	for _, ranks := range []int{0, 1} {
		m := ReplicaMetrics{EngineRanks: ranks, QueueLength: 7, KvCacheUsage: 0.4,
			TotalKvCapacityTokens: 1000, TokensInUse: 400,
			MaxRankQueueLength: 99, MaxRankKvCacheUsage: 0.99}
		if got := m.BusiestRankQueue(); got != 7 {
			t.Errorf("ranks %d: BusiestRankQueue = %d, want the record's queue 7", ranks, got)
		}
		if got := m.BusiestRankKvUsage(); got != 0.4 {
			t.Errorf("ranks %d: BusiestRankKvUsage = %v, want the record's 0.4", ranks, got)
		}
		if got := m.BusiestRankTokens(); got != 400 {
			t.Errorf("ranks %d: BusiestRankTokens = %d, want TokensInUse 400", ranks, got)
		}
	}
}

// A record of eight ranks reads its busiest rank, and its tokens are that
// rank's utilization scaled to the record's capacity.
func TestBusiestRankOfEightRanks(t *testing.T) {
	m := ReplicaMetrics{EngineRanks: 8, QueueLength: 8, KvCacheUsage: 0.125,
		TotalKvCapacityTokens: 8000, TokensInUse: 1000,
		MaxRankQueueLength: 5, MaxRankKvCacheUsage: 1.0}
	if got := m.BusiestRankQueue(); got != 5 {
		t.Errorf("BusiestRankQueue = %d, want 5", got)
	}
	if got := m.BusiestRankKvUsage(); got != 1.0 {
		t.Errorf("BusiestRankKvUsage = %v, want 1.0", got)
	}
	if got := m.BusiestRankTokens(); got != 8000 {
		t.Errorf("BusiestRankTokens = %d, want 8000: one full rank, as the pod", got)
	}
	// An idle pod is idle on every rank.
	idle := ReplicaMetrics{EngineRanks: 8, TotalKvCapacityTokens: 8000}
	if q, kv, tok := idle.BusiestRankQueue(), idle.BusiestRankKvUsage(), idle.BusiestRankTokens(); q != 0 || kv != 0 || tok != 0 {
		t.Errorf("idle pod reads busiest rank %d / %v / %d, want zeros", q, kv, tok)
	}
}

// Without a busiest-rank reading, the busiest rank is read at the least it can
// hold: the mean, rounded up for the queue. It never reads below the mean.
func TestBusiestRankWithoutAReading(t *testing.T) {
	m := ReplicaMetrics{EngineRanks: 8, QueueLength: 9, KvCacheUsage: 0.25,
		TotalKvCapacityTokens: 8000, TokensInUse: 2000}
	if got := m.BusiestRankQueue(); got != 2 {
		t.Errorf("BusiestRankQueue = %d, want ceil(9/8) = 2", got)
	}
	if got := m.BusiestRankKvUsage(); got != 0.25 {
		t.Errorf("BusiestRankKvUsage = %v, want the mean 0.25", got)
	}
	if got := m.BusiestRankTokens(); got != 2000 {
		t.Errorf("BusiestRankTokens = %d, want TokensInUse 2000", got)
	}
	// A reading above the cache is clamped to it, as TokensInUse is.
	m.MaxRankKvCacheUsage = 1.5
	if got := m.BusiestRankTokens(); got != 8000 {
		t.Errorf("BusiestRankTokens = %d, want the capacity 8000", got)
	}
}
