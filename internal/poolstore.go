package internal

import (
	"context"
	"log"
	"time"
)

const dbSyncInterval = 60 * time.Second

// PoolStore syncs in-memory pool/token state to the SQLite database.
// It runs as a background goroutine and writes every dbSyncInterval.
// It does NOT read from the DB — that is a future step (warm-start on restart).
type PoolStore struct {
	db       *DB
	registry *PoolRegistry
	tokens   *TokenRegistry
}

func NewPoolStore(db *DB, registry *PoolRegistry, tokens *TokenRegistry) *PoolStore {
	return &PoolStore{db: db, registry: registry, tokens: tokens}
}

// Run syncs state to the DB every dbSyncInterval until ctx is cancelled.
func (ps *PoolStore) Run(ctx context.Context) {
	// Initial sync after a short delay so subgraph seed has time to run first.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	ps.sync()

	ticker := time.NewTicker(dbSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ps.sync()
		}
	}
}

func (ps *PoolStore) sync() {
	t0 := time.Now()

	// Tokens first so that any pool FK lookups in future queries work.
	tokenErrs := 0
	for _, t := range ps.tokens.All() {
		if err := ps.db.UpsertToken(t); err != nil {
			tokenErrs++
		}
	}

	poolErrs := 0
	for _, p := range ps.registry.All() {
		if err := ps.db.UpsertPool(p); err != nil {
			poolErrs++
		}
	}

	pools, _ := ps.db.PoolCount()
	live, _ := ps.db.LivePoolCount()
	tokens, _ := ps.db.TokenCount()
	elapsed := time.Since(t0).Round(time.Millisecond)

	if tokenErrs > 0 || poolErrs > 0 {
		log.Printf("[poolstore] sync done in %s — %d tokens, %d pools (%d live), %d errs",
			elapsed, tokens, pools, live, tokenErrs+poolErrs)
	} else {
		log.Printf("[poolstore] sync done in %s — %d tokens, %d pools (%d live)",
			elapsed, tokens, pools, live)
	}
}
