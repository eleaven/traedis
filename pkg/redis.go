// Why a hand-written client instead of github.com/redis/go-redis: Traefik runs
// the plugin through Yaegi v0.16.1, which cannot load go-redis (checked with
// v9.8.0, the first with HSETEX, up to v9.23.0):
//   - it imports unsafe and syscall, which Yaegi does not expose to plugins by
//     default; even with unsafe allowed, recent versions need unsafe.String,
//     which Yaegi lacks, and a Go version newer than the Go 1.22 stdlib it exposes;
//   - its connection pool relies on select statements, which Yaegi runs
//     incorrectly from several goroutines (see redisClient);
//   - it would have to be vendored: tens of thousands of lines, for HMGET,
//     HSETEX, HPEXPIRE, HDEL, HLEN and DEL.

package traedis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"
)

const (
	// maxLineBytes bounds RESP status lines; longer lines are a protocol error.
	maxLineBytes = 4096
	// maxArrayItems bounds RESP arrays: the replies to HMGET and HPEXPIRE have
	// one item per field asked for.
	maxArrayItems = 16
	// largeArgBytes is the size from which an argument is written as it is,
	// instead of being copied into the command (see writeCommand).
	largeArgBytes = 16 << 10
	logInterval   = time.Minute
)

var (
	errProtocol      = errors.New("redis: protocol error")
	errTooLarge      = errors.New("redis: value too large")
	errPoolExhausted = errors.New("redis: pool exhausted")
	// errUnavailable fails the commands that are not sent (see do).
	errUnavailable = errors.New("redis: unavailable")
)

// redisError is an error reply sent by the server.
type redisError string

func (e redisError) Error() string { return "redis: " + string(e) }

// redisClient is a minimal RESP2 client with a pool of at most redis.poolSize
// connections.
// Every operation is bounded by redis.timeout, and getting its connection by
// redis.dialTimeout: Redis has more time to send a body than to show it is
// there.
//
// The pool uses no select statement: Yaegi v0.16.1 shares the operands of a
// select between the goroutines running it, which hands one connection to two
// requests.
type redisClient struct {
	// name is the one of the middleware, which the log lines start with; out
	// is where they go.
	name    string
	out     io.Writer
	opts    redisOptions
	maxBulk int64
	sem     chan struct{} // one token per connection in use
	// takeToken is the select that takes a token of sem if one is free, built
	// once: its first case is the send to sem, its second one the default.
	takeToken []reflect.SelectCase
	idleMu    sync.Mutex
	idle      []*redisConn // at most redis.poolSize: only token holders add to it
	// The breaker (see do): failures counts the commands that failed in a row,
	// up to redis.maxFailures; from there, retryAt is when the next command
	// may be sent, and downSince when it was reached.
	stateMu   sync.Mutex
	failures  int
	retryAt   time.Time
	downSince time.Time
	logMu     sync.Mutex
	lastLog   time.Time
	logCount  int
}

type redisConn struct {
	nc net.Conn
	r  *bufio.Reader
	// reused is set once the connection has been through the pool: it may have
	// been closed since, by Redis or on the way to it.
	reused bool
}

type reply struct {
	kind byte // '+', '-', ':', '*'
	str  string
	num  int64
	// items holds the bulk strings of an array: nil for a null one, and for an
	// integer.
	items [][]byte
}

func newRedisClient(name string, opts redisOptions, maxBulk int64) *redisClient {
	sem := make(chan struct{}, opts.poolSize)
	return &redisClient{
		name:    name,
		out:     os.Stderr,
		opts:    opts,
		maxBulk: maxBulk,
		sem:     sem,
		takeToken: []reflect.SelectCase{
			{Dir: reflect.SelectSend, Chan: reflect.ValueOf(sem), Send: reflect.ValueOf(struct{}{})},
			{Dir: reflect.SelectDefault},
		},
	}
}

func (c *redisClient) get(ctx context.Context, key string, fields []string) ([][]byte, error) {
	args := make([][]byte, 0, 2+len(fields))
	args = append(args, []byte("HMGET"), []byte(key))
	for _, f := range fields {
		args = append(args, []byte(f))
	}
	r, err := do(ctx, c, '*', args)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			// Not a value of ours: nothing usable is stored.
			return make([][]byte, len(fields)), nil
		}
		return nil, err
	}
	if len(r.items) != len(fields) {
		return nil, errProtocol
	}
	return r.items, nil
}

// set runs HSETEX: with replace (FXX), Redis stores none of the fields unless
// they all exist already.
func (c *redisClient) set(ctx context.Context, key string, fields []fieldValue, ttl time.Duration, replace bool) error {
	ms := ttl.Milliseconds()
	if ms <= 0 {
		return nil
	}
	args := [][]byte{[]byte("HSETEX"), []byte(key)}
	if replace {
		args = append(args, []byte("FXX"))
	}
	args = append(args, []byte("PX"), []byte(strconv.FormatInt(ms, 10)),
		[]byte("FIELDS"), []byte(strconv.Itoa(len(fields))))
	for _, f := range fields {
		args = append(args, []byte(f.name), f.value)
	}
	_, err := do(ctx, c, ':', args)
	return err
}

func (c *redisClient) expire(ctx context.Context, key, field string, ttl time.Duration) error {
	ms := ttl.Milliseconds()
	if ms <= 0 {
		return nil
	}
	_, err := do(ctx, c, '*', [][]byte{[]byte("HPEXPIRE"), []byte(key), []byte(strconv.FormatInt(ms, 10)),
		[]byte("FIELDS"), []byte("1"), []byte(field)})
	return err
}

func (c *redisClient) invalidate(ctx context.Context, key string) error {
	_, err := do(ctx, c, ':', [][]byte{[]byte("DEL"), []byte(key)})
	return err
}

func (c *redisClient) count(ctx context.Context, key string) (int, error) {
	r, err := do(ctx, c, ':', [][]byte{[]byte("HLEN"), []byte(key)})
	return int(r.num), err
}

func (c *redisClient) del(ctx context.Context, key string, fields []string) error {
	args := make([][]byte, 0, 2+len(fields))
	args = append(args, []byte("HDEL"), []byte(key))
	for _, f := range fields {
		args = append(args, []byte(f))
	}
	_, err := do(ctx, c, ':', args)
	return err
}

// do runs one command, within redis.timeout, and expects a reply of the given
// kind: any other one is errProtocol. Any failure but an error reply or a
// reply of another kind closes the connection: its state is unknown. A command that fails on a
// connection of the pool is run once more, on a new one (see stale).
//
// After redis.maxFailures failures in a row, Redis is given up on: commands
// fail at once with errUnavailable, instead of making every request wait for a
// timeout. One of them is let through every redis.retryAfter, the others
// still failing while it runs: Redis is back when it succeeds.
//
// Like acquire, release and roundTrip, which run for every command too, it is
// a function rather than a method of the client: under Yaegi, calling a method
// costs several times what calling a function does.
func do(ctx context.Context, c *redisClient, kind byte, args [][]byte) (reply, error) {
	c.stateMu.Lock()
	failures := c.failures
	if c.opts.maxFailures > 0 && failures >= c.opts.maxFailures {
		now := time.Now()
		if now.Before(c.retryAt) {
			c.stateMu.Unlock()
			return reply{}, errUnavailable
		}
		c.retryAt = now.Add(c.opts.retryAfter)
	}
	c.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.opts.timeout)
	defer cancel()

	var r reply
	conn, err := acquire(ctx, c)
	if err == nil {
		r, err = roundTrip(ctx, conn, c.maxBulk, args)
		if err != nil && conn.reused && stale(err) {
			_ = conn.nc.Close()
			dialCtx, cancelDial := context.WithTimeout(ctx, c.opts.dialTimeout)
			conn, err = c.dial(dialCtx)
			cancelDial()
			if err == nil {
				r, err = roundTrip(ctx, conn, c.maxBulk, args)
			}
		}
		if err != nil {
			release(c, nil)
			if conn != nil {
				_ = conn.nc.Close()
			}
		} else {
			release(c, conn)
		}
	}
	// A value that is too large is still an answer.
	if err != nil && !errors.Is(err, errTooLarge) {
		c.failed(err)
		return reply{}, err
	}
	if failures > 0 {
		c.answered()
	}
	if err != nil {
		return reply{}, err
	}
	if r.kind == '-' {
		err := redisError(r.str)
		c.logError(err)
		return reply{}, err
	}
	if r.kind != kind {
		return reply{}, errProtocol
	}
	return r, nil
}

// failed counts a command that Redis did not answer, for the breaker of do.
// Neither a full pool nor a client that went away says anything of Redis.
func (c *redisClient) failed(err error) {
	c.logError(err)
	if c.opts.maxFailures <= 0 || errors.Is(err, errPoolExhausted) || errors.Is(err, context.Canceled) {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	now := time.Now()
	c.retryAt = now.Add(c.opts.retryAfter)
	if c.failures >= c.opts.maxFailures {
		return
	}
	c.failures++
	if c.failures == c.opts.maxFailures {
		c.downSince = now
		logf(c.out, c.name, "redis: no answer to %d commands in a row: the cache is bypassed, and Redis tried again every %v: %v",
			c.failures, c.opts.retryAfter, err)
	}
}

// answered ends a series of failures, and tells when Redis was given up on.
func (c *redisClient) answered() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.opts.maxFailures > 0 && c.failures >= c.opts.maxFailures {
		logf(c.out, c.name, "redis: answers again after %v: the cache is back", time.Since(c.downSince).Round(time.Millisecond))
	}
	c.failures = 0
}

// stale reports whether err, the failure of a command on a connection of the
// pool, may only mean that the connection was closed while it was idle (Redis
// restarted, a firewall dropped it): the command is then worth running again
// on a new one, which all of ours can be. Not when Redis answered, whatever it
// said, nor when it did not in time: there is none left.
func stale(err error) bool {
	return !errors.Is(err, errTooLarge) && !errors.Is(err, errProtocol) && !errors.Is(err, os.ErrDeadlineExceeded)
}

// acquire returns an idle connection or dials a new one, once it has a pool
// token. Waiting for the token and dialing are given redis.dialTimeout, which
// a free token and an idle connection, the usual case, never need.
func acquire(ctx context.Context, c *redisClient) (*redisConn, error) {
	// select { case c.sem <- struct{}{}: default: }
	chosen, _, _ := reflect.Select(c.takeToken)
	if chosen == 0 {
		// As idleConn, without the cost of its call under Yaegi.
		var conn *redisConn
		c.idleMu.Lock()
		if n := len(c.idle); n > 0 {
			conn = c.idle[n-1]
			c.idle = c.idle[:n-1]
		}
		c.idleMu.Unlock()
		if conn != nil {
			return conn, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.dialTimeout)
	defer cancel()
	if chosen != 0 {
		// select { case c.sem <- struct{}{}: case <-ctx.Done(): }
		chosen, _, _ = reflect.Select([]reflect.SelectCase{
			c.takeToken[0],
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
		})
		if chosen != 0 {
			return nil, errPoolExhausted
		}
		if conn := idleConn(c); conn != nil {
			return conn, nil
		}
	}

	conn, err := c.dial(ctx)
	if err != nil {
		<-c.sem
		return nil, err
	}
	return conn, nil
}

// idleConn takes the idle connection used last out of the pool, if there is one.
func idleConn(c *redisClient) *redisConn {
	var conn *redisConn
	c.idleMu.Lock()
	if n := len(c.idle); n > 0 {
		conn = c.idle[n-1]
		c.idle = c.idle[:n-1]
	}
	c.idleMu.Unlock()
	return conn
}

// release returns a healthy connection to the pool (nil for a closed one).
func release(c *redisClient, conn *redisConn) {
	if conn != nil {
		conn.reused = true
		c.idleMu.Lock()
		c.idle = append(c.idle, conn)
		c.idleMu.Unlock()
	}
	<-c.sem
}

// dial opens a connection and sets it up (TLS, AUTH, SELECT), until the
// deadline of ctx.
func (c *redisClient) dial(ctx context.Context) (*redisConn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.opts.addr)
	if err != nil {
		return nil, err
	}
	if c.opts.tls != nil {
		// Nothing is sent, the password least of all, before Redis is known to
		// be the one that is meant.
		secured := tls.Client(nc, c.opts.tls)
		if err := secured.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: TLS: %w", err)
		}
		nc = secured
	}
	conn := &redisConn{nc: nc, r: bufio.NewReaderSize(nc, maxLineBytes)}

	var setup [][][]byte
	if c.opts.password != "" {
		if c.opts.username != "" {
			setup = append(setup, [][]byte{[]byte("AUTH"), []byte(c.opts.username), []byte(c.opts.password)})
		} else {
			setup = append(setup, [][]byte{[]byte("AUTH"), []byte(c.opts.password)})
		}
	}
	if c.opts.db != 0 {
		setup = append(setup, [][]byte{[]byte("SELECT"), []byte(strconv.Itoa(c.opts.db))})
	}
	for _, args := range setup {
		r, err := roundTrip(ctx, conn, c.maxBulk, args)
		if err == nil && r.kind == '-' {
			err = redisError(r.str)
		}
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: %s: %w", args[0], err)
		}
	}
	return conn, nil
}

// roundTrip sends a command and reads its reply, until the deadline of ctx.
func roundTrip(ctx context.Context, conn *redisConn, maxBulk int64, args [][]byte) (reply, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.nc.SetDeadline(deadline); err != nil {
			return reply{}, err
		}
	}
	if err := writeCommand(conn.nc, args); err != nil {
		return reply{}, err
	}
	return readReply(conn.r, maxBulk)
}

// writeCommand sends a command in one write. Its large arguments, the bodies,
// are not copied into it: they go to the connection as they are.
func writeCommand(nc net.Conn, args [][]byte) error {
	size := 16
	for _, a := range args {
		size += 16
		if len(a) < largeArgBytes {
			size += len(a)
		}
	}
	var bufs net.Buffers
	b := make([]byte, 0, size)
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		if len(a) >= largeArgBytes {
			// What follows it goes on in the rest of the same buffer.
			bufs = append(bufs, b, a)
			b = b[len(b):]
		} else {
			b = append(b, a...)
		}
		b = append(b, '\r', '\n')
	}
	if bufs == nil {
		_, err := nc.Write(b)
		return err
	}
	bufs = append(bufs, b)
	_, err := bufs.WriteTo(nc)
	return err
}

// readReply reads one RESP2 reply: a status, an error, an integer, or an array
// of bulk strings and integers, which is all that our commands get. The server
// is not trusted: line lengths and array lengths are bounded, and bulk lengths
// are checked against maxBulk before allocating.
//
// It is one loop over the lines of the reply, the first one then one per item
// of an array, rather than a function called for each value: under Yaegi, the
// calls would cost more than the parsing.
func readReply(r *bufio.Reader, maxBulk int64) (reply, error) {
	var out reply
	for i := -1; i < len(out.items); i++ {
		line, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return reply{}, errProtocol
			}
			return reply{}, err
		}
		if len(line) < 3 || line[len(line)-2] != '\r' {
			return reply{}, errProtocol
		}
		kind := line[0]
		payload := string(line[1 : len(line)-2])

		if i >= 0 {
			// An item of the array: nothing is kept of an integer, nor of a
			// null bulk string.
			if kind == ':' || (kind == '$' && payload == "-1") {
				continue
			}
			n, err := strconv.ParseInt(payload, 10, 64)
			if kind != '$' || err != nil || n < 0 {
				return reply{}, errProtocol
			}
			if n > maxBulk {
				return reply{}, errTooLarge
			}
			buf := make([]byte, n+2)
			if _, err := io.ReadFull(r, buf); err != nil {
				return reply{}, err
			}
			if buf[n] != '\r' || buf[n+1] != '\n' {
				return reply{}, errProtocol
			}
			out.items[i] = buf[:n]
			continue
		}

		switch kind {
		case '+', '-':
			return reply{kind: kind, str: payload}, nil
		case ':', '*':
			n, err := strconv.ParseInt(payload, 10, 64)
			if err != nil {
				return reply{}, errProtocol
			}
			if kind == ':' {
				return reply{kind: kind, num: n}, nil
			}
			if n < 0 || n > maxArrayItems {
				return reply{}, errProtocol
			}
			out.kind = kind
			out.items = make([][]byte, n)
		default:
			return reply{}, errProtocol
		}
	}
	return out, nil
}

// logError reports Redis failures at most once per logInterval, with the number
// of errors since the last report. It never logs the DSN.
func (c *redisClient) logError(err error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.logCount++
	now := time.Now()
	if now.Sub(c.lastLog) < logInterval {
		return
	}
	logf(c.out, c.name, "redis error, bypassing the cache (%d errors since last report): %v", c.logCount, err)
	c.lastLog = now
	c.logCount = 0
}

// logf writes a line to out, the log of Traefik: it starts with the name of
// the middleware, which tells its Redis and its routers from the others.
func logf(out io.Writer, name, format string, args ...any) {
	_, _ = fmt.Fprintf(out, "traedis: "+name+": "+format+"\n", args...)
}
