package filter

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Request bodies: what bounds the memory they take and the time a slow one
// holds anything (round-5 R5-E-5, R5-E-6).
//
//   - A body is read before its request takes a class slot, so a client that
//     dribbles its body holds no slot of any class: only its connection.
//   - The read has a deadline set from the body's size at a minimum rate
//     (readGrace + size/minRate), so a dribble is cut off in seconds, not
//     at the server's 20 s read timeout.
//   - Memory is reserved from one process-wide byte budget as the bytes
//     arrive (a client that declares 1 MiB and sends nothing holds nothing),
//     and stays reserved until the request is answered, copies included:
//     each body is charged factor x its size, where factor counts the copies
//     its handler makes while parsing and re-encoding it. When the budget is
//     spent, a request is refused 503 rather than queued.
//   - Bytes past the first smallBody of a request may use only 3/4 of the
//     budget, so a flood of large bodies cannot starve the small ones (every
//     JSON-RPC call, and a typical private tx broadcast, is under it).
//   - The backend has its own budget.
//
// So the edge's heap from bodies is at most bodyBudget + backendBudget,
// whatever arrives, well under GOMEMLIMIT (160 MiB) and the 192 MiB limit.
const (
	// maxBody bounds a request body: a JSON-RPC call or an LCD broadcast or
	// simulate. CometBFT's own default (rpc max_body_bytes) is 1 MB; a
	// private tx with several ~15 KB proofs is far below it.
	maxBody       = 1 << 20
	smallBody     = 256 << 10
	bodyBudget    = 48 << 20
	backendBudget = 16 << 20
	minRate       = 128 << 10 // bytes per second
	readGrace     = 2 * time.Second
	readChunk     = 32 << 10

	// Copies each body has live at its peak, counted generously: the read
	// buffer and its growth slack, plus for JSON-RPC the params, the decoded
	// argument and the re-encoded request.
	factorJSONRPC = 6
	factorLCD     = 3
)

type budget struct {
	mu        sync.Mutex
	used, big int64
	max       int64
	bigMax    int64
}

func newBudget(max int64) *budget { return &budget{max: max, bigMax: max * 3 / 4} }

// take reserves n bytes, big of which are past a request's first smallBody.
func (b *budget) take(n, big int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max || b.big+big > b.bigMax {
		return false
	}
	b.used += n
	b.big += big
	return true
}

func (b *budget) give(n, big int64) {
	b.mu.Lock()
	b.used -= n
	b.big -= big
	b.mu.Unlock()
}

// inUse reports the bytes reserved (for tests).
func (b *budget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

var (
	publicBodies  = newBudget(bodyBudget)
	backendBodies = newBudget(backendBudget)
)

// readDeadline: how long a body of n bytes may take to arrive.
func readDeadline(n int64) time.Duration {
	return readGrace + time.Duration(n)*time.Second/minRate
}

// readBody reads at most maxBody bytes of the request body under a deadline
// set from its size, reserving factor bytes of budget per byte as they
// arrive. It returns the body and the release of its reservation, which the
// caller defers until the request is answered; or an HTTP status.
func readBody(w http.ResponseWriter, r *http.Request, factor int64, backend bool) ([]byte, func(), int) {
	if r.ContentLength > maxBody {
		return nil, nil, http.StatusRequestEntityTooLarge
	}
	b := publicBodies
	if backend {
		b = backendBodies
	}
	expect := int64(maxBody)
	if r.ContentLength >= 0 {
		expect = r.ContentLength
	}
	rc := http.NewResponseController(w)
	// ErrNotSupported only off a real connection (tests): the server's
	// ReadTimeout still applies there.
	_ = rc.SetReadDeadline(time.Now().Add(readDeadline(expect)))
	// Left in place: the server resets it for the connection's next request,
	// and after this handler it bounds the server's own drain of an unread
	// body (clearing it here would let that drain wait for ever).

	var held, heldBig, total int64
	release := func() { b.give(held, heldBig) }
	// The body arrives into chunks, each reserved before it is allocated,
	// and is joined once at the end: no growth copies left as garbage.
	var chunks [][]byte
	first := int64(readChunk)
	if expect < first {
		first = expect + 1 // room to see EOF without a second chunk
	}
	for {
		var cur []byte
		if n := len(chunks); n > 0 && len(chunks[n-1]) < cap(chunks[n-1]) {
			cur = chunks[n-1]
		} else {
			size := int64(readChunk)
			if n == 0 {
				size = first
			}
			var big int64
			if total+size > smallBody {
				big = size * factor
			}
			if !b.take(size*factor, big) {
				release()
				return nil, nil, http.StatusServiceUnavailable
			}
			held += size * factor
			heldBig += big
			cur = make([]byte, 0, size)
			chunks = append(chunks, cur)
		}
		k, err := r.Body.Read(cur[len(cur):cap(cur)])
		chunks[len(chunks)-1] = cur[:len(cur)+k]
		total += int64(k)
		if total > maxBody {
			release()
			return nil, nil, http.StatusRequestEntityTooLarge
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			release()
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, nil, http.StatusRequestTimeout
			}
			return nil, nil, http.StatusBadRequest
		}
	}
	if len(chunks) == 1 {
		return chunks[0], release, 0
	}
	body := make([]byte, 0, total)
	for _, c := range chunks {
		body = append(body, c...)
	}
	return body, release, 0
}
