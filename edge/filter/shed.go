package filter

import (
	"sync"
	"time"
)

// Expensive-answer memory (round-8 R8-D-1). A tx by hash and a height's
// block_results are answers whose size was fixed when the chain stored
// them: the same request always gets the same answer, byte for byte. The
// node builds such an answer whole, in memory several times over (round-6
// R6-E-1), before the edge sees a byte of it, so what the edge can do is
// remember what an answer turned out to be and not let it be built again
// on the terms of a cheap one:
//
//   - over: the answer passed its ceiling (forward.go). Public requests for
//     it are refused (502 "answer too large", what the ceiling gave)
//     without a slot and without asking the node. With the ceilings derived
//     from the chain's caps this only happens if those numbers are wrong.
//   - heavy: the answer was more than heavyAnswer bytes. Public requests
//     for it are served from the bulk class (2 slots, 1 per client) instead
//     of their own: a tx by hash is in txhash because a commit poll is a
//     millisecond point read, and a tx whose answer is megabytes (a relay
//     tx of a few MB of results, or a contract call built to be loud, each
//     paid for once and askable for ever) must not hold those slots for
//     the price of one. Only its first build, per form and per shedTTL,
//     happens in txhash.
//
// A mark comes from the node's own answer, whoever asked (the backend's
// reads mark too, though the backend is never refused or moved), so no
// client can mark a tx or height for anyone else. What is held: the kind
// of answer and the tx hash or block height (e.g. "rpc-tx/<hash>",
// "block_results/<height>"), the mark and its expiry. Nothing about the
// client. In memory only, never logged, at most shedMax entries (the
// soonest to expire goes first when full). block_results without a height
// (the latest block) is not keyed: what it names changes every block.
//
// The TTL is short because nothing else bounds how long an entry lives,
// not because an answer could change: a key re-learnt after it expires
// costs one more build.
const (
	shedTTL     = 10 * time.Minute
	shedMax     = 4096
	heavyAnswer = 1 << 20
)

type answerMark uint8

const (
	markNone answerMark = iota
	markHeavy
	markOver
)

type shedList struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	keys map[string]shedEntry
}

type shedEntry struct {
	exp  time.Time
	mark answerMark
}

func newShedList(ttl time.Duration, max int) *shedList {
	return &shedList{ttl: ttl, max: max, keys: map[string]shedEntry{}}
}

// get: key's mark, markNone if unmarked or expired.
func (s *shedList) get(key string) answerMark {
	if s == nil || key == "" {
		return markNone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.keys[key]
	if !ok {
		return markNone
	}
	if time.Now().After(e.exp) {
		delete(s.keys, key)
		return markNone
	}
	return e.mark
}

// has: key is marked over its ceiling.
func (s *shedList) has(key string) bool { return s.get(key) == markOver }

// add marks key over its ceiling for the TTL.
func (s *shedList) add(key string) { s.mark(key, markOver) }

// mark records m for key for the TTL; a key's mark never goes down while
// it lives (over stays over).
func (s *shedList) mark(key string, m answerMark) {
	if s == nil || key == "" || m == markNone {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	e, ok := s.keys[key]
	if ok && now.Before(e.exp) && e.mark > m {
		m = e.mark
	}
	if !ok && len(s.keys) >= s.max {
		var oldest string
		var oldestExp time.Time
		for k, x := range s.keys {
			if now.After(x.exp) {
				delete(s.keys, k)
				continue
			}
			if oldest == "" || x.exp.Before(oldestExp) {
				oldest, oldestExp = k, x.exp
			}
		}
		if len(s.keys) >= s.max {
			delete(s.keys, oldest)
		}
	}
	s.keys[key] = shedEntry{exp: now.Add(s.ttl), mark: m}
}

// size reports the entries held (tests).
func (s *shedList) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}
