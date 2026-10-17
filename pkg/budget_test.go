package traedis

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTakeAndGiveBytes(t *testing.T) {
	b := &budget{max: 100}
	if !takeBytes(b, 60) || takeBytes(b, 41) || !takeBytes(b, 40) {
		t.Fatalf("used = %d: takes past max must fail, the others succeed", b.used)
	}
	giveBytes(b, 100)
	if b.used != 0 {
		t.Errorf("used = %d after giving everything back", b.used)
	}
	if !takeBytes(nil, 1<<40) {
		t.Error("a nil budget has no bound")
	}
}

func TestHoldCopy(t *testing.T) {
	t.Run("small copies are not counted", func(t *testing.T) {
		rec := &recorder{max: 1 << 20, bodies: &budget{max: 0}}
		if !holdCopy(rec, smallBody) || rec.held != 0 {
			t.Errorf("held = %d", rec.held)
		}
	})
	t.Run("a growing copy takes from the budget a few times", func(t *testing.T) {
		rec := &recorder{max: 1 << 20, bodies: &budget{max: 1 << 30}}
		takes := 0
		for size := int64(64 << 10); size <= 1<<20; size += 32 << 10 {
			held := rec.held
			if !holdCopy(rec, size) || rec.held < size {
				t.Fatalf("size %d: held = %d", size, rec.held)
			}
			if rec.held != held {
				takes++
			}
		}
		if takes > 6 || rec.held != 1<<20 || rec.bodies.used != rec.held {
			t.Errorf("takes = %d, held = %d, used = %d: as much again each time, up to max", takes, rec.held, rec.bodies.used)
		}
	})
	t.Run("only what it needs when the budget has no room for more", func(t *testing.T) {
		rec := &recorder{max: 1 << 20, bodies: &budget{max: 100 << 10}}
		for _, size := range []int64{40 << 10, 70 << 10, 90 << 10} {
			if !holdCopy(rec, size) {
				t.Fatalf("size %d: refused with %d used", size, rec.bodies.used)
			}
		}
		if rec.held != 90<<10 {
			t.Errorf("held = %d, want %d", rec.held, 90<<10)
		}
		if holdCopy(rec, 110<<10) || rec.held != 90<<10 {
			t.Errorf("past the budget: held = %d", rec.held)
		}
	})
}

// A copy that stops is given back at once, not when its response ends: that may be long after, for a stream.
func TestStoppedCopyIsGivenBack(t *testing.T) {
	rec := newRecorder(httptest.NewRecorder(), &hookStub{capture: true}, 100<<10, &budget{max: 1 << 20})
	chunk := []byte(strings.Repeat("x", 40<<10))
	_, _ = rec.Write(chunk)
	_, _ = rec.Write(chunk)
	if rec.bodies.used == 0 {
		t.Fatal("a copy over smallBody must hold some of the budget")
	}
	_, _ = rec.Write(chunk) // past max
	if rec.bodies.used != 0 || rec.held != 0 {
		t.Errorf("used = %d, held = %d once the copy stopped", rec.bodies.used, rec.held)
	}
}

// fill takes what is left of the budget of c, as other requests would. The returned function gives it back.
func fill(c *cache) func() {
	c.bodies.mu.Lock()
	n := c.bodies.max - c.bodies.used
	c.bodies.used = c.bodies.max
	c.bodies.mu.Unlock()
	return func() { giveBytes(c.bodies, n) }
}

func checkBudgetEmpty(t *testing.T, c *cache) {
	t.Helper()
	if c.bodies.used != 0 {
		t.Errorf("budget used = %d once requests ended, want 0", c.bodies.used)
	}
}

var largeBody = strings.Repeat("x", 64<<10)

func TestCopyOverBudgetIsNotStored(t *testing.T) {
	for _, withLength := range []bool{true, false} {
		t.Run("Content-Length "+strconv.FormatBool(withLength), func(t *testing.T) {
			b := &backend{header: http.Header{"Cache-Control": {"max-age=60"}}, body: largeBody}
			if withLength {
				b.header.Set("Content-Length", strconv.Itoa(len(largeBody)))
			}
			c, st := newTestCache(t, b, nil)
			release := fill(c)
			rec := doRequest(c, http.MethodGet, testURL, nil)
			if rec.Body.String() != largeBody || lastCacheStatus(rec) != "traedis; fwd=uri-miss; fwd-status=200" {
				t.Errorf("the response must reach the client as it is: %d bytes, Cache-Status = %q", rec.Body.Len(), lastCacheStatus(rec))
			}
			if st.sets != 0 {
				t.Errorf("sets = %d: no room for the copy, nothing to store", st.sets)
			}
			release()
			checkBudgetEmpty(t, c)

			doRequest(c, http.MethodGet, testURL, nil)
			if st.sets != 1 {
				t.Errorf("sets = %d once there is room again", st.sets)
			}
			checkBudgetEmpty(t, c)
		})
	}
}

// A copy that is not stored gives back what it held: incomplete, or not written because a write to its key is running.
func TestCopyNotStoredIsGivenBack(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		b := &backend{
			header: http.Header{"Cache-Control": {"max-age=60"}, "Content-Length": {strconv.Itoa(len(largeBody) + 1)}},
			body:   largeBody,
		}
		c, st := newTestCache(t, b, nil)
		doRequest(c, http.MethodGet, testURL, nil)
		if st.sets != 0 {
			t.Errorf("sets = %d", st.sets)
		}
		checkBudgetEmpty(t, c)
	})
	t.Run("write not run", func(t *testing.T) {
		b := &backend{header: http.Header{"Cache-Control": {"max-age=60"}}, body: largeBody}
		c, st := newTestCache(t, b, nil)
		running := &pendingWrite{done: make(chan struct{})}
		c.writing[cacheKey(testURL)] = running
		c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, testURL, nil))
		delete(c.writing, cacheKey(testURL))
		if st.sets != 0 {
			t.Errorf("sets = %d", st.sets)
		}
		checkBudgetEmpty(t, c)
	})
}

func TestHitOverBudgetIsForwarded(t *testing.T) {
	b := &backend{header: http.Header{"Cache-Control": {"max-age=60"}}, body: largeBody}
	c, st := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	release := fill(c)

	rec := doRequest(c, http.MethodGet, testURL, nil)
	if b.calls != 2 || rec.Body.String() != largeBody {
		t.Errorf("backend calls = %d, %d bytes: the request is forwarded", b.calls, rec.Body.Len())
	}
	if got := lastCacheStatus(rec); got != "traedis; fwd=bypass; fwd-status=200; detail=memory" {
		t.Errorf("Cache-Status = %q", got)
	}
	if st.sets != 1 {
		t.Errorf("sets = %d: a bypass stores nothing", st.sets)
	}
	// Without the body, there is nothing to hold.
	for _, hdr := range []http.Header{nil, {"If-None-Match": {`"x"`}}} {
		method := http.MethodHead
		if hdr != nil {
			method = http.MethodGet
		}
		stored := st.entry(cacheKey(testURL), "")
		storeEntry(t, st, testURL, &entry{
			status:       200,
			header:       http.Header{"Cache-Control": {"max-age=60"}, "Etag": {`"x"`}},
			body:         []byte(largeBody),
			requestTime:  stored.requestTime,
			responseTime: stored.responseTime,
		})
		if rec := doRequest(c, method, testURL, hdr); !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
			t.Errorf("%s %v: Cache-Status = %q", method, hdr, lastCacheStatus(rec))
		}
	}
	release()
	if rec := doRequest(c, http.MethodGet, testURL, nil); !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") ||
		rec.Body.String() != largeBody {
		t.Errorf("once there is room again: Cache-Status = %q", lastCacheStatus(rec))
	}
	checkBudgetEmpty(t, c)
}

func TestSmallHitIsNotCounted(t *testing.T) {
	b := cacheableBackend()
	c, _ := newTestCache(t, b, nil)
	doRequest(c, http.MethodGet, testURL, nil)
	release := fill(c)
	defer release()
	if rec := doRequest(c, http.MethodGet, testURL, nil); !strings.HasPrefix(lastCacheStatus(rec), "traedis; hit") {
		t.Errorf("Cache-Status = %q: a small body is not counted", lastCacheStatus(rec))
	}
}

// Whatever the way a stored body is sent, what it held is given back.
func TestBudgetGivenBack(t *testing.T) {
	stale := func(cacheControl string) *entry {
		at := time.Now().Add(-70 * time.Second)
		return &entry{
			status:       200,
			header:       http.Header{"Cache-Control": {cacheControl}, "Etag": {`"x"`}},
			body:         []byte(largeBody),
			requestTime:  at,
			responseTime: at,
		}
	}
	tests := []struct {
		name    string
		stored  *entry
		backend *backend
		reqHdr  http.Header
		want    string
	}{
		{name: "stale-while-revalidate", stored: stale("max-age=60, stale-while-revalidate=30"),
			backend: &backend{header: http.Header{"Cache-Control": {"max-age=60"}}, body: largeBody},
			want:    "traedis; hit; ttl=-"},
		{name: "validated by a 304", stored: stale("max-age=60"),
			backend: &backend{status: 304, header: http.Header{"Etag": {`"x"`}, "Cache-Control": {"max-age=60"}}},
			want:    "traedis; fwd=stale; fwd-status=304; ttl="},
		{name: "validated by a 304, read again", stored: stale("max-age=60"), reqHdr: http.Header{"If-None-Match": {`"y"`}},
			backend: &backend{status: 304, header: http.Header{"Etag": {`"x"`}, "Cache-Control": {"max-age=60"}}},
			want:    "traedis; fwd=stale; fwd-status=304; ttl="},
		{name: "stale-if-error", stored: stale("max-age=60, stale-if-error=30"),
			backend: &backend{status: 503},
			want:    "traedis; fwd=stale; fwd-status=503; ttl=-"},
		{name: "replaced", stored: stale("max-age=60"),
			backend: &backend{header: http.Header{"Cache-Control": {"max-age=60"}}, body: largeBody + "y"},
			want:    "traedis; fwd=stale; fwd-status=200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.backend
			c, st := newTestCache(t, b, nil)
			storeEntry(t, st, testURL, tt.stored)
			rec := doRequest(c, http.MethodGet, testURL, tt.reqHdr)
			waitRevalidations(t, c)
			settle(c)
			if got := lastCacheStatus(rec); !strings.HasPrefix(got, tt.want) {
				t.Errorf("Cache-Status = %q, want %q…", got, tt.want)
			}
			if rec.Body.Len() < len(largeBody) {
				t.Errorf("body: %d bytes", rec.Body.Len())
			}
			checkBudgetEmpty(t, c)
		})
	}
}

// The proxy of Traefik panics with http.ErrAbortHandler when it cannot read all the body of the backend, which a client that goes away
// causes: what the request held of the budget must be given back all the same. Otherwise a few aborted downloads would hold all of it, and
// the cache would be bypassed for good.
func TestBudgetGivenBackOnPanic(t *testing.T) {
	tests := []struct {
		name   string
		length bool // the response has a Content-Length
		stale  bool // a stale response with a large body is stored
	}{
		{name: "copy of known length", length: true},
		{name: "copy of unknown length"},
		{name: "stored body waiting for the backend", length: true, stale: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=60")
				if tt.length {
					w.Header().Set("Content-Length", strconv.Itoa(2*len(largeBody)))
				}
				_, _ = w.Write([]byte(largeBody))
				panic(http.ErrAbortHandler)
			})
			c, st := newTestCache(t, b, nil)
			if tt.stale {
				at := time.Now().Add(-2 * time.Minute)
				storeEntry(t, st, testURL, &entry{status: 200, header: http.Header{"Cache-Control": {"max-age=60"}, "Etag": {`"v1"`}},
					body: []byte(largeBody), requestTime: at, responseTime: at})
			}
			func() {
				defer func() {
					// Compared by its text: Yaegi wraps what is recovered.
					if p := recover(); fmt.Sprint(p) != http.ErrAbortHandler.Error() {
						t.Errorf("panic = %v, want http.ErrAbortHandler", p)
					}
				}()
				c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, testURL, nil))
			}()
			checkBudgetEmpty(t, c)
		})
	}
}
