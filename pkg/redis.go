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
	redisPoolSize = 10
	// maxLineBytes bounds RESP status lines; longer lines are a protocol error.
	maxLineBytes = 4096
	// maxArrayItems bounds RESP arrays: the replies to HMGET and HPEXPIRE have
	// one item per field asked for.
	maxArrayItems = 16
	// largeArgBytes is the size from which an argument is written as it is,
	// instead of being copied into the command (see encodeLargeCommand).
	largeArgBytes = 16 << 10
	logInterval   = time.Minute
)

var (
	errProtocol = errors.New("redis: protocol error")
	errTooLarge = errors.New("redis: value too large")
)

// redisError is an error reply sent by the server.
type redisError string

func (e redisError) Error() string { return "redis: " + string(e) }

// redisClient is a minimal RESP2 client with a bounded connection pool.
// Every operation is bounded by redis.timeout, and getting its connection by
// redis.dialTimeout: Redis has more time to send a body than to show it is
// there.
//
// The pool uses no select statement: Yaegi v0.16.1 shares the operands of a
// select between the goroutines running it, which hands one connection to two
// requests.
type redisClient struct {
	opts    redisOptions
	maxBulk int64
	sem     chan struct{} // one token per connection in use
	// takeToken is the select that takes a token of sem if one is free, built
	// once: its first case is the send to sem, its second one the default.
	takeToken []reflect.SelectCase
	idleMu    sync.Mutex
	idle      []*redisConn // at most redisPoolSize: only token holders add to it
	logMu     sync.Mutex
	lastLog   time.Time
	logCount  int
}

type redisConn struct {
	nc net.Conn
	r  *bufio.Reader
}

type reply struct {
	kind byte // '+', '-', ':', '*'
	str  string
	num  int64
	// items holds the bulk strings of an array: nil for a null one, and for an
	// integer.
	items [][]byte
}

func newRedisClient(opts redisOptions, maxBulk int64) *redisClient {
	sem := make(chan struct{}, redisPoolSize)
	return &redisClient{
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
	r, err := do(ctx, c, args)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			// Not a value of ours: nothing usable is stored.
			return make([][]byte, len(fields)), nil
		}
		return nil, err
	}
	if r.kind != '*' || len(r.items) != len(fields) {
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
	r, err := do(ctx, c, args)
	if err != nil {
		return err
	}
	if r.kind != ':' {
		return errProtocol
	}
	return nil
}

func (c *redisClient) expire(ctx context.Context, key, field string, ttl time.Duration) error {
	ms := ttl.Milliseconds()
	if ms <= 0 {
		return nil
	}
	r, err := do(ctx, c, [][]byte{[]byte("HPEXPIRE"), []byte(key), []byte(strconv.FormatInt(ms, 10)),
		[]byte("FIELDS"), []byte("1"), []byte(field)})
	if err != nil {
		return err
	}
	if r.kind != '*' {
		return errProtocol
	}
	return nil
}

func (c *redisClient) invalidate(ctx context.Context, key string) error {
	r, err := do(ctx, c, [][]byte{[]byte("DEL"), []byte(key)})
	if err != nil {
		return err
	}
	if r.kind != ':' {
		return errProtocol
	}
	return nil
}

func (c *redisClient) count(ctx context.Context, key string) (int, error) {
	r, err := do(ctx, c, [][]byte{[]byte("HLEN"), []byte(key)})
	if err != nil {
		return 0, err
	}
	if r.kind != ':' {
		return 0, errProtocol
	}
	n := int(r.num)
	return n, nil
}

func (c *redisClient) del(ctx context.Context, key string, fields []string) error {
	args := make([][]byte, 0, 2+len(fields))
	args = append(args, []byte("HDEL"), []byte(key))
	for _, f := range fields {
		args = append(args, []byte(f))
	}
	r, err := do(ctx, c, args)
	if err != nil {
		return err
	}
	if r.kind != ':' {
		return errProtocol
	}
	return nil
}

// do runs one command, within redis.timeout. Any failure but an error reply
// closes the connection: its state is unknown.
//
// Like acquire, release and roundTrip, which run for every command too, it is
// a function rather than a method of the client: under Yaegi, calling a method
// costs several times what calling a function does.
func do(ctx context.Context, c *redisClient, args [][]byte) (reply, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.timeout)
	defer cancel()

	conn, err := acquire(ctx, c)
	if err != nil {
		c.logError(err)
		return reply{}, err
	}
	r, err := roundTrip(ctx, conn, c.maxBulk, args)
	if err != nil {
		release(c, nil)
		_ = conn.nc.Close()
		if !errors.Is(err, errTooLarge) {
			c.logError(err)
		}
		return reply{}, err
	}
	release(c, conn)
	if r.kind == '-' {
		err := redisError(r.str)
		c.logError(err)
		return reply{}, err
	}
	return r, nil
}

// acquire returns an idle connection or dials a new one, once it has a pool
// token. Waiting for the token and dialing are given redis.dialTimeout, which
// a free token and an idle connection, the usual case, never need.
func acquire(ctx context.Context, c *redisClient) (*redisConn, error) {
	// select { case c.sem <- struct{}{}: default: }
	chosen, _, _ := reflect.Select(c.takeToken)
	if chosen == 0 {
		if conn := idleConn(c); conn != nil {
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
			return nil, fmt.Errorf("redis: pool exhausted: %w", ctx.Err())
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
		c.idleMu.Lock()
		c.idle = append(c.idle, conn)
		c.idleMu.Unlock()
	}
	<-c.sem
}

// dial opens a connection and sets it up (AUTH, SELECT), until the deadline of
// ctx.
func (c *redisClient) dial(ctx context.Context) (*redisConn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.opts.addr)
	if err != nil {
		return nil, err
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

// writeCommand sends a command: in one buffer, unless it is large (see
// encodeLargeCommand).
func writeCommand(nc net.Conn, args [][]byte) error {
	size := 16
	for _, a := range args {
		size += len(a) + 16
	}
	if size >= largeArgBytes {
		command := encodeLargeCommand(args)
		_, err := command.WriteTo(nc)
		return err
	}
	b := make([]byte, 0, size)
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	_, err := nc.Write(b)
	return err
}

// encodeLargeCommand encodes a command without copying its large arguments,
// the bodies: they go to the connection as they are, in one write with the
// rest.
func encodeLargeCommand(args [][]byte) net.Buffers {
	var bufs net.Buffers
	b := make([]byte, 0, 256)
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		if len(a) >= largeArgBytes {
			bufs = append(bufs, b, a)
			b = make([]byte, 0, 256)
		} else {
			b = append(b, a...)
		}
		b = append(b, '\r', '\n')
	}
	return append(bufs, b)
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
	_, _ = fmt.Fprintf(os.Stderr, "traedis: redis error, bypassing the cache (%d errors since last report): %v\n", c.logCount, err)
	c.lastLog = now
	c.logCount = 0
}
