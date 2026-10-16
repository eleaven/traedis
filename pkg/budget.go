package traedis

import (
	"net/http"
	"sync"
)

// smallBody is the size up to which a body held in memory does not count in
// the budget: no more than what Traefik buffers to proxy a response anyway.
const smallBody = 32 << 10

// budget bounds the bodies a middleware holds in memory at once, for all its
// requests: the copies of responses to store (see recorder), and the stored
// bodies being sent (see holdBody). Without it, clients would choose how much
// memory Traefik uses: many slow requests for large responses, each one held
// in full. A body larger than smallBody takes its size from it, until it is
// let go; past maxBufferedBytes, a response is not copied (it is sent, but not
// stored), nor sent from the cache (the request is forwarded).
//
// Its functions are not methods: under Yaegi, calling a method costs several
// times what calling a function does.
type budget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

// takeBytes takes n bytes from b, and reports whether it had them. A nil
// budget has no bound.
func takeBytes(b *budget, n int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	ok := b.used+n <= b.max
	if ok {
		b.used += n
	}
	b.mu.Unlock()
	return ok
}

// giveBytes gives n bytes back to b.
func giveBytes(b *budget, n int64) {
	if b == nil || n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// holdBody takes the size of the body of e from b, when the answer to r sends
// it and it is not small, unless e holds it already: before the body is read
// from the store, or once it was read along with its entry. It reports
// whether it could: otherwise, the body is not to be read nor sent. letGo
// gives it back.
func holdBody(b *budget, r *http.Request, e *entry) bool {
	if e.held > 0 || e.size <= smallBody || !sendsBody(r, e) {
		return true
	}
	if !takeBytes(b, e.size) {
		return false
	}
	e.held = e.size
	return true
}

// letGo gives back to b what the body of e holds, if anything.
func letGo(b *budget, e *entry) {
	if e != nil && e.held > 0 {
		giveBytes(b, e.held)
		e.held = 0
	}
}

// holdCopy makes the copy of rec hold size bytes of its budget, once it is
// larger than smallBody. It reports whether it could: otherwise, the response
// is not to be copied any more. A copy of unknown length, which grows write
// after write, takes as much again as it holds, up to max: not a lock and a
// call per write. Only what it needs when the budget has no more room.
func holdCopy(rec *recorder, size int64) bool {
	if size <= smallBody || size <= rec.held {
		return true
	}
	need := size - rec.held
	more := need
	if more < rec.held {
		more = rec.held
	}
	if more < smallBody {
		more = smallBody
	}
	if rec.held+more > rec.max {
		more = rec.max - rec.held
	}
	if more > need && !takeBytes(rec.bodies, more) {
		more = need
	}
	if more == need && !takeBytes(rec.bodies, more) {
		return false
	}
	rec.held += more
	return true
}
