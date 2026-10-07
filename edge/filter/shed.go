package filter

import (
	"sync"
	"time"
)

// Expensive-answer shedding (round-8 R8-D-1). A tx by hash and a height's
// block_results are answers whose size was fixed when the chain stored
// them: the same request always gets the same answer, byte for byte. The
// ceiling (forward.go) stops the copy of one that is too large, but only
// after the node has built it whole, in memory several times over (round-6
// R6-E-1). Without memory here, a client could ask for the same oversized
// answer again and again, and the node would build it every time to have
// it thrown away at the edge.
//
// So once a public answer passes its ceiling, its key is remembered for
// shedTTL, and public requests for that key are refused (502 "answer too
// large", the same answer the ceiling gave) without a slot and without
// asking the node. The backend has no ceiling and is never shed.
//
// What is remembered: the kind of answer and the tx hash or block height
// (e.g. "rpc-tx/<hash>", "block_results/<height>") and when it expires.
// Nothing about the client that asked. In memory only, never logged, at
// most shedMax entries (the oldest goes first when full). An answer cannot
// be marked oversized unless it is: the mark is the node's own answer
// passing the ceiling, so no client can shed a tx or height for anyone
// else. block_results without a height (the latest block) is not keyed
// and not shed: what it names changes every block.
//
// The TTL is short because nothing else bounds how long an entry lives,
// not because the answer could shrink: a key re-learnt after it expires
// costs one more build.
const (
	shedTTL = 10 * time.Minute
	shedMax = 4096
)

type shedList struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	keys map[string]time.Time // key -> expiry
}

func newShedList(ttl time.Duration, max int) *shedList {
	return &shedList{ttl: ttl, max: max, keys: map[string]time.Time{}}
}

// has: key is marked oversized and the mark has not expired.
func (s *shedList) has(key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.keys[key]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.keys, key)
		return false
	}
	return true
}

// add marks key oversized for the TTL.
func (s *shedList) add(key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if _, ok := s.keys[key]; !ok && len(s.keys) >= s.max {
		var oldest string
		var oldestExp time.Time
		for k, exp := range s.keys {
			if now.After(exp) {
				delete(s.keys, k)
				continue
			}
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		if len(s.keys) >= s.max {
			delete(s.keys, oldest)
		}
	}
	s.keys[key] = now.Add(s.ttl)
}

// size reports the entries held (tests).
func (s *shedList) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}
