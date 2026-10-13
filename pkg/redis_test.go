package traedis

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoKey, as a reply, answers a command with its key, as the value of the one
// field it asks for.
const echoKey = "echo-key"

// fakeRedis is a scripted RESP server: replies maps a command name to its raw
// reply (defaultReplies otherwise); an empty reply means never answering.
type fakeRedis struct {
	ln      net.Listener
	replies map[string]string
	// delay is how long every reply takes.
	delay    time.Duration
	mu       sync.Mutex
	commands [][]string
	accepts  int
	conns    []net.Conn
}

var defaultReplies = map[string]string{
	"HMGET":    "*1\r\n$5\r\nhello\r\n",
	"HSETEX":   ":1\r\n",
	"HPEXPIRE": "*1\r\n:1\r\n",
	"HDEL":     ":1\r\n",
	"DEL":      ":1\r\n",
	"HLEN":     ":3\r\n",
	"AUTH":     "+OK\r\n",
	"SELECT":   "+OK\r\n",
}

func newFakeRedis(t *testing.T, replies map[string]string) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{}
	for k, v := range defaultReplies {
		all[k] = v
	}
	for k, v := range replies {
		all[k] = v
	}
	f := &fakeRedis{ln: ln, replies: all}
	go f.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.conns {
			_ = c.Close()
		}
	})
	return f
}

func (f *fakeRedis) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.accepts++
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.handle(c)
	}
}

func (f *fakeRedis) handle(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, args)
		resp := f.replies[args[0]]
		f.mu.Unlock()
		if resp == "" {
			continue // never answer
		}
		if resp == echoKey {
			resp = "*1\r\n$" + strconv.Itoa(len(args[1])) + "\r\n" + args[1] + "\r\n"
		}
		time.Sleep(f.delay)
		if _, err := io.WriteString(c, resp); err != nil {
			return
		}
	}
}

// reply changes the reply to a command.
func (f *fakeRedis) reply(command, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[command] = raw
}

// drop closes the connections of the clients, as a Redis that restarts does.
func (f *fakeRedis) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
}

func (f *fakeRedis) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepts
}

func (f *fakeRedis) received() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.commands...)
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func testClient(f *fakeRedis) *redisClient {
	return newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 200 * time.Millisecond, timeout: 200 * time.Millisecond}, 1024)
}

// getField reads one field of key: nil when it is not there.
func getField(c *redisClient, key, field string) ([]byte, error) {
	values, err := c.get(context.Background(), key, []string{field})
	if err != nil {
		return nil, err
	}
	return values[0], nil
}

// fieldValues returns fields from their names and values (name, value…).
func fieldValues(namesAndValues ...string) []fieldValue {
	var fields []fieldValue
	for i := 0; i < len(namesAndValues); i += 2 {
		fields = append(fields, fieldValue{name: namesAndValues[i], value: []byte(namesAndValues[i+1])})
	}
	return fields
}

func TestRedisGetAndSet(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := testClient(f)
	ctx := context.Background()

	v, err := getField(c, "traedis:k", "")
	if err != nil || string(v) != "hello" {
		t.Fatalf("get() = %q, %v", v, err)
	}
	if err := c.set(ctx, "traedis:k", fieldValues("", "v\r\n1"), 1500*time.Millisecond, false); err != nil {
		t.Fatalf("set() = %v", err)
	}

	accepts := f.connections()
	cmds := f.received()
	want := []string{
		"HMGET traedis:k ",
		"HSETEX traedis:k PX 1500 FIELDS 1  v\r\n1",
	}
	if len(cmds) != len(want) {
		t.Fatalf("commands = %q", cmds)
	}
	for i, w := range want {
		if got := strings.Join(cmds[i], " "); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
	if accepts != 1 {
		t.Errorf("connections = %d, want 1 (pooled)", accepts)
	}
}

// The fields of a response are read in one round trip: a field that is not
// there is nil, which an empty one is not.
func TestRedisGetSeveralFields(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": "*3\r\n$5\r\nentry\r\n$-1\r\n$0\r\n\r\n"})
	values, err := testClient(f).get(context.Background(), "traedis:k", []string{"", "f1", "f1:body"})
	if err != nil || len(values) != 3 {
		t.Fatalf("get() = %q, %v", values, err)
	}
	if string(values[0]) != "entry" || values[1] != nil || values[2] == nil || len(values[2]) != 0 {
		t.Errorf("get() = %q, want a value, nil and an empty value", values)
	}
	cmds := f.received()
	if len(cmds) != 1 || strings.Join(cmds[0], " ") != "HMGET traedis:k  f1 f1:body" {
		t.Errorf("commands = %q", cmds)
	}
}

func TestRedisDel(t *testing.T) {
	f := newFakeRedis(t, nil)
	if err := testClient(f).del(context.Background(), "traedis:k", []string{"f1", "f1:body"}); err != nil {
		t.Fatalf("del() = %v", err)
	}
	cmds := f.received()
	if len(cmds) != 1 || strings.Join(cmds[0], " ") != "HDEL traedis:k f1 f1:body" {
		t.Errorf("commands = %q", cmds)
	}
}

func TestRedisSetSeveralFields(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := testClient(f)
	ctx := context.Background()

	if err := c.set(ctx, "traedis:k", fieldValues("", "marker", "f1", "v\r\n1", "f1:body", "body"), 1500*time.Millisecond, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.set(ctx, "traedis:k", fieldValues("", "marker", "f1", "v2"), time.Second, true); err != nil {
		t.Fatalf("set() replacing = %v", err)
	}
	if err := c.set(ctx, "traedis:k", fieldValues("", "marker", "f1", "v3"), 0, false); err != nil {
		t.Fatalf("set() without ttl = %v", err)
	}

	cmds := f.received()
	want := []string{
		// The marker, the variant and its body in one command: they expire together.
		"HSETEX traedis:k PX 1500 FIELDS 3  marker f1 v\r\n1 f1:body body",
		"HSETEX traedis:k FXX PX 1000 FIELDS 2  marker f1 v2",
	}
	if len(cmds) != len(want) {
		t.Fatalf("commands = %q", cmds)
	}
	for i, w := range want {
		if got := strings.Join(cmds[i], " "); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// A body is written to the connection as it is, not copied into the command:
// what Redis gets is the same.
func TestRedisSetLargeValue(t *testing.T) {
	f := newFakeRedis(t, nil)
	body := strings.Repeat("0123456789abcdef", 4<<10) // 64 KiB
	for _, value := range []string{body, body[:largeArgBytes], body[:largeArgBytes-1], ""} {
		if err := testClient(f).set(context.Background(), "traedis:k", fieldValues("", "entry", ":body", value), time.Second, false); err != nil {
			t.Fatalf("set() = %v", err)
		}
		cmds := f.received()
		want := []string{"HSETEX", "traedis:k", "PX", "1000", "FIELDS", "2", "", "entry", ":body", value}
		if got := cmds[len(cmds)-1]; strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("a value of %d bytes: command = %.80q…", len(value), got)
		}
	}
}

func TestRedisExpire(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := testClient(f)
	ctx := context.Background()
	if err := c.expire(ctx, "traedis:k", "f1:body", 1500*time.Millisecond); err != nil {
		t.Fatalf("expire() = %v", err)
	}
	if err := c.expire(ctx, "traedis:k", "f1:body", 0); err != nil {
		t.Fatalf("expire() without ttl = %v", err)
	}
	cmds := f.received()
	if len(cmds) != 1 || strings.Join(cmds[0], " ") != "HPEXPIRE traedis:k 1500 FIELDS 1 f1:body" {
		t.Errorf("commands = %q", cmds)
	}

	f = newFakeRedis(t, map[string]string{"HPEXPIRE": ":1\r\n"})
	if err := testClient(f).expire(ctx, "traedis:k", "f1:body", time.Second); !errors.Is(err, errProtocol) {
		t.Errorf("expire() error = %v, want errProtocol", err)
	}
}

func TestRedisCount(t *testing.T) {
	f := newFakeRedis(t, nil)
	n, err := testClient(f).count(context.Background(), "traedis:k")
	if err != nil || n != 3 {
		t.Fatalf("count() = %d, %v, want 3", n, err)
	}
	cmds := f.received()
	if len(cmds) != 1 || strings.Join(cmds[0], " ") != "HLEN traedis:k" {
		t.Errorf("commands = %q", cmds)
	}

	f = newFakeRedis(t, map[string]string{"HLEN": "+OK\r\n"})
	if _, err := testClient(f).count(context.Background(), "traedis:k"); !errors.Is(err, errProtocol) {
		t.Errorf("count() error = %v, want errProtocol", err)
	}
}

func TestRedisInvalidate(t *testing.T) {
	f := newFakeRedis(t, nil)
	if err := testClient(f).invalidate(context.Background(), "traedis:k"); err != nil {
		t.Fatalf("invalidate() = %v", err)
	}
	cmds := f.received()
	if len(cmds) != 1 || strings.Join(cmds[0], " ") != "DEL traedis:k" {
		t.Errorf("commands = %q", cmds)
	}

	f = newFakeRedis(t, map[string]string{"DEL": "+OK\r\n"})
	if err := testClient(f).invalidate(context.Background(), "traedis:k"); !errors.Is(err, errProtocol) {
		t.Errorf("invalidate() error = %v, want errProtocol", err)
	}
}

func TestRedisGetNullIsMiss(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": "*1\r\n$-1\r\n"})
	if v, err := getField(testClient(f), "k", ""); v != nil || err != nil {
		t.Fatalf("get() = %q, %v, want no value", v, err)
	}
}

func TestRedisDialSendsAuthAndSelect(t *testing.T) {
	tests := []struct {
		name string
		opts redisOptions
		want []string
	}{
		{name: "ACL user and database", opts: redisOptions{username: "u", password: "p", db: 15}, want: []string{"AUTH u p", "SELECT 15", "HMGET k "}},
		{name: "password only", opts: redisOptions{password: "p"}, want: []string{"AUTH p", "HMGET k "}},
		{name: "no credentials, database 0", opts: redisOptions{}, want: []string{"HMGET k "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRedis(t, nil)
			opts := tt.opts
			opts.addr = f.ln.Addr().String()
			opts.dialTimeout = 200 * time.Millisecond
			opts.timeout = 200 * time.Millisecond
			if _, err := getField(newRedisClient(opts, 1024), "k", ""); err != nil {
				t.Fatal(err)
			}
			cmds := f.received()
			var got []string
			for _, c := range cmds {
				got = append(got, strings.Join(c, " "))
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("commands = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedisAuthFailure(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"AUTH": "-WRONGPASS invalid username-password pair\r\n"})
	opts := redisOptions{addr: f.ln.Addr().String(), password: "s3cret", dialTimeout: 200 * time.Millisecond, timeout: 200 * time.Millisecond}
	_, err := getField(newRedisClient(opts, 1024), "k", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestRedisErrorReplyKeepsConnection(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HSETEX": "-ERR unknown command 'HSETEX'\r\n"})
	c := testClient(f)
	ctx := context.Background()
	if err := c.set(ctx, "k", fieldValues("", "v"), time.Second, false); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("set() error = %v, want the server error", err)
	}
	if _, err := getField(c, "k", ""); err != nil {
		t.Fatal(err)
	}
	if accepts := f.connections(); accepts != 1 {
		t.Errorf("connections = %d, want 1", accepts)
	}
}

func TestRedisUntrustedReplies(t *testing.T) {
	tests := []struct {
		name    string
		reply   string
		wantErr error
		// kept: the reply was read to its end, the connection is used again.
		kept bool
	}{
		{name: "unknown reply type", reply: "?what\r\n", wantErr: errProtocol},
		{name: "missing CR", reply: "*1\n$5\nhello\n", wantErr: errProtocol},
		{name: "oversized bulk is a miss without allocating", reply: "*1\r\n$999999999999\r\n"},
		{name: "negative bulk length", reply: "*1\r\n$-7\r\n", wantErr: errProtocol},
		{name: "bulk without trailing CRLF", reply: "*1\r\n$2\r\nhello\r\n", wantErr: errProtocol},
		{name: "status line too long", reply: "+" + strings.Repeat("a", 2*maxLineBytes) + "\r\n", wantErr: errProtocol},
		{name: "integer instead of an array", reply: ":1\r\n", wantErr: errProtocol, kept: true},
		{name: "bulk instead of an array", reply: "$5\r\nhello\r\n", wantErr: errProtocol},
		{name: "status in an array", reply: "*1\r\n+OK\r\n", wantErr: errProtocol},
		{name: "integer in an array is no value", reply: "*1\r\n:1\r\n", kept: true},
		{name: "empty array", reply: "*0\r\n", wantErr: errProtocol, kept: true},
		{name: "array of another length", reply: "*2\r\n$1\r\na\r\n$1\r\nb\r\n", wantErr: errProtocol, kept: true},
		{name: "oversized array, without allocating", reply: "*999999999999\r\n", wantErr: errProtocol},
		{name: "negative array length", reply: "*-1\r\n", wantErr: errProtocol},
		{name: "array of arrays", reply: "*1\r\n*1\r\n$1\r\na\r\n", wantErr: errProtocol},
		{name: "truncated array", reply: "*2\r\n$1\r\na\r\n?\r\n", wantErr: errProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRedis(t, map[string]string{"HMGET": tt.reply})
			c := testClient(f)
			if v, err := getField(c, "k", ""); !errors.Is(err, tt.wantErr) || v != nil {
				t.Fatalf("first get() = %q, %v, want no value and %v", v, err, tt.wantErr)
			}
			_, _ = getField(c, "k", "")
			// After a reply that was not read to its end, the connection is
			// not reused.
			want := 2
			if tt.kept {
				want = 1
			}
			if accepts := f.connections(); accepts != want {
				t.Errorf("connections = %d, want %d", accepts, want)
			}
		})
	}
}

func TestRedisTimeout(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": ""})
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 50 * time.Millisecond, timeout: 50 * time.Millisecond}, 1024)
	start := time.Now()
	if _, err := getField(c, "k", ""); err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("get() took %v, want about redis.timeout", elapsed)
	}
}

// redis.timeout is for the whole operation: a reply may take longer than
// redis.dialTimeout, which is only for getting the connection.
func TestRedisSlowReply(t *testing.T) {
	f := newFakeRedis(t, nil)
	f.delay = 100 * time.Millisecond
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 20 * time.Millisecond, timeout: 5 * time.Second}, 1024)
	for i := 0; i < 2; i++ { // on a new connection, then on the idle one
		if v, err := getField(c, "k", ""); err != nil || string(v) != "hello" {
			t.Fatalf("get() = %q, %v", v, err)
		}
	}
}

// Setting up a new connection is part of getting it.
func TestRedisDialTimeout(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"AUTH": ""})
	opts := redisOptions{addr: f.ln.Addr().String(), password: "p", dialTimeout: 50 * time.Millisecond, timeout: 5 * time.Second}
	c := newRedisClient(opts, 1024)
	start := time.Now()
	if _, err := getField(c, "k", ""); err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("get() took %v, want about redis.dialTimeout", elapsed)
	}
	if len(c.sem) != 0 {
		t.Errorf("pool tokens leaked: %d", len(c.sem))
	}
}

// A connection of the pool that was closed while idle does not cost a bypass:
// the command is run again on a new one.
func TestRedisRetriesOnAStaleConnection(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := testClient(f)
	ctx := context.Background()
	if _, err := getField(c, "k", ""); err != nil {
		t.Fatal(err)
	}
	f.drop()
	if v, err := getField(c, "k", ""); err != nil || string(v) != "hello" {
		t.Fatalf("get() on a closed connection = %q, %v, want the value", v, err)
	}
	f.drop()
	if err := c.set(ctx, "k", fieldValues("", "v"), time.Second, false); err != nil {
		t.Fatalf("set() on a closed connection = %v", err)
	}
	if accepts := f.connections(); accepts != 3 {
		t.Errorf("connections = %d, want 3", accepts)
	}
	if len(c.sem) != 0 || len(c.idle) != 1 {
		t.Errorf("pool: %d tokens taken, %d idle connections, want 0 and 1", len(c.sem), len(c.idle))
	}
}

// Only once, and not when Redis is gone: the cache is bypassed.
func TestRedisRetryFails(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := testClient(f)
	if _, err := getField(c, "k", ""); err != nil {
		t.Fatal(err)
	}
	_ = f.ln.Close()
	f.drop()
	if _, err := getField(c, "k", ""); err == nil {
		t.Fatal("expected an error")
	}
	if len(c.sem) != 0 || len(c.idle) != 0 {
		t.Errorf("pool: %d tokens taken, %d idle connections, want none", len(c.sem), len(c.idle))
	}
}

// A reply that does not come in time is not waited for twice.
func TestRedisTimeoutIsNotRetried(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HLEN": ""})
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 50 * time.Millisecond, timeout: 50 * time.Millisecond}, 1024)
	if _, err := getField(c, "k", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.count(context.Background(), "k"); err == nil {
		t.Fatal("expected a timeout")
	}
	if accepts := f.connections(); accepts != 1 {
		t.Errorf("connections = %d, want 1", accepts)
	}
}

// breakerClient gives up on Redis after 3 failures, for 100 ms.
func breakerClient(f *fakeRedis) *redisClient {
	return newRedisClient(redisOptions{
		addr: f.ln.Addr().String(), dialTimeout: 200 * time.Millisecond, timeout: 200 * time.Millisecond,
		maxFailures: 3, retryAfter: 100 * time.Millisecond,
	}, 1024)
}

// After redis.maxFailures failures in a row, Redis is no longer asked: the
// commands fail at once. One is sent every redis.retryAfter, and all of them
// again once it is answered.
func TestRedisBreaker(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": "?broken\r\n"})
	c := breakerClient(f)
	for i := 0; i < 3; i++ {
		if _, err := getField(c, "k", ""); !errors.Is(err, errProtocol) {
			t.Fatalf("get() %d = %v, want errProtocol", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := getField(c, "k", ""); !errors.Is(err, errUnavailable) {
			t.Fatalf("get() once Redis is given up on = %v, want errUnavailable", err)
		}
	}
	if n := len(f.received()); n != 3 {
		t.Errorf("commands sent = %d, want 3", n)
	}

	// Tried again, in vain: not before another redis.retryAfter.
	time.Sleep(120 * time.Millisecond)
	if _, err := getField(c, "k", ""); !errors.Is(err, errProtocol) {
		t.Fatalf("get() after redis.retryAfter = %v, want errProtocol", err)
	}
	if _, err := getField(c, "k", ""); !errors.Is(err, errUnavailable) {
		t.Fatalf("get() after a failed attempt = %v, want errUnavailable", err)
	}

	f.reply("HMGET", defaultReplies["HMGET"])
	if _, err := getField(c, "k", ""); !errors.Is(err, errUnavailable) {
		t.Fatalf("get() before redis.retryAfter = %v, want errUnavailable", err)
	}
	time.Sleep(120 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if v, err := getField(c, "k", ""); err != nil || string(v) != "hello" {
			t.Fatalf("get() %d once Redis is back = %q, %v", i, v, err)
		}
	}
	if n := len(f.received()); n != 7 {
		t.Errorf("commands sent = %d, want 7", n)
	}
}

// While the command that tries Redis again waits for its reply, the others
// still fail at once.
func TestRedisBreakerTriesOneCommand(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": "?broken\r\n"})
	c := breakerClient(f)
	for i := 0; i < 3; i++ {
		_, _ = getField(c, "k", "")
	}
	f.reply("HMGET", defaultReplies["HMGET"])
	f.delay = 50 * time.Millisecond
	time.Sleep(120 * time.Millisecond)

	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := getField(c, "k", "")
			errs <- err
		}()
	}
	answered := 0
	for i := 0; i < 8; i++ {
		if err := <-errs; err == nil {
			answered++
		} else if !errors.Is(err, errUnavailable) {
			t.Errorf("get() = %v, want errUnavailable", err)
		}
	}
	if n := len(f.received()); answered != 1 || n != 4 {
		t.Errorf("%d commands answered and %d sent, want 1 and 4", answered, n)
	}
}

// Only failures in a row count, and only those of Redis: an error reply and a
// value that is too large are answers, a full pool says nothing of it.
func TestRedisBreakerStaysClosed(t *testing.T) {
	f := newFakeRedis(t, map[string]string{
		"HLEN":   "?broken\r\n",
		"HSETEX": "-ERR unknown command 'HSETEX'\r\n",
		"HMGET":  "*1\r\n$999999999999\r\n",
	})
	c := breakerClient(f)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = c.count(ctx, "k")
		_, _ = c.count(ctx, "k")
		if i%2 == 0 {
			_ = c.set(ctx, "k", fieldValues("", "v"), time.Second, false)
		} else if _, err := getField(c, "k", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.received()); n != 15 {
		t.Errorf("commands sent = %d, want 15", n)
	}

	for i := 0; i < redisPoolSize; i++ {
		c.sem <- struct{}{}
	}
	c.opts.dialTimeout = 5 * time.Millisecond
	for i := 0; i < 5; i++ {
		if _, err := c.count(ctx, "k"); !errors.Is(err, errPoolExhausted) {
			t.Fatalf("count() with a full pool = %v, want errPoolExhausted", err)
		}
	}
}

// redis.maxFailures 0: every command tries Redis.
func TestRedisBreakerDisabled(t *testing.T) {
	f := newFakeRedis(t, map[string]string{"HMGET": "?broken\r\n"})
	c := testClient(f)
	for i := 0; i < 10; i++ {
		if _, err := getField(c, "k", ""); !errors.Is(err, errProtocol) {
			t.Fatalf("get() %d = %v, want errProtocol", i, err)
		}
	}
}

func TestRedisPoolExhausted(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 50 * time.Millisecond, timeout: 5 * time.Second}, 1024)
	for i := 0; i < redisPoolSize; i++ {
		c.sem <- struct{}{}
	}
	start := time.Now()
	if _, err := getField(c, "k", ""); err == nil {
		t.Fatal("expected an error when the pool is exhausted")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("get() took %v, want about redis.dialTimeout", elapsed)
	}
	if accepts := f.connections(); accepts != 0 {
		t.Errorf("connections = %d, want 0", accepts)
	}
}

func TestRedisPoolWaitsForAConnection(t *testing.T) {
	f := newFakeRedis(t, nil)
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 2 * time.Second, timeout: 2 * time.Second}, 1024)
	for i := 0; i < redisPoolSize; i++ {
		c.sem <- struct{}{}
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		<-c.sem
	}()
	start := time.Now()
	if v, err := getField(c, "k", ""); err != nil || string(v) != "hello" {
		t.Fatalf("get() = %q, %v, want the value once a connection is free", v, err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("get() took %v: it did not wait for the pool", elapsed)
	}
}

// Regression test, failing under `yaegi test` only: Yaegi v0.16.1 shares the
// operands of a select statement between the goroutines running it, so the pool
// handed one connection to two requests, which read each other's replies.
func TestRedisPoolConcurrency(t *testing.T) {
	const workers, rounds = 32, 100
	f := newFakeRedis(t, map[string]string{"HMGET": echoKey})
	c := newRedisClient(redisOptions{addr: f.ln.Addr().String(), dialTimeout: 5 * time.Second, timeout: 5 * time.Second}, 1024)

	failures := make(chan string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				v, err := getField(c, key, "")
				if err != nil || string(v) != key {
					failures <- "get(" + key + ") = " + strconv.Quote(string(v)) + ", " + fmt.Sprint(err)
					return
				}
			}
		}("key-" + strconv.Itoa(w))
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if accepts := f.connections(); accepts > redisPoolSize {
		t.Errorf("connections = %d, want at most %d", accepts, redisPoolSize)
	}
}

func TestRedisUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	c := newRedisClient(redisOptions{addr: addr, dialTimeout: 50 * time.Millisecond, timeout: 50 * time.Millisecond}, 1024)
	if _, err := getField(c, "k", ""); err == nil {
		t.Fatal("expected an error")
	}
	if len(c.sem) != 0 {
		t.Errorf("pool tokens leaked: %d", len(c.sem))
	}
}

// Integration tests: only with TRAEDIS_REDIS_DSN (Redis >= 8), on keys they create.
func integrationClient(t *testing.T) *redisClient {
	t.Helper()
	dsn := os.Getenv("TRAEDIS_REDIS_DSN")
	if dsn == "" {
		t.Skip("TRAEDIS_REDIS_DSN not set")
	}
	opts, err := parseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	opts.dialTimeout = time.Second
	opts.timeout = time.Second
	return newRedisClient(opts, 1<<20)
}

func testKey(t *testing.T) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return keyPrefix + "test:" + hex.EncodeToString(b)
}

func TestRedisIntegrationRoundTripAndExpiry(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)

	if v, err := getField(c, key, ""); v != nil || err != nil {
		t.Fatalf("get() on a new key = %q, %v, want no value", v, err)
	}
	value := "binary\x00\r\nvalue"
	if err := c.set(ctx, key, fieldValues("", value), 300*time.Millisecond, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	got, err := getField(c, key, "")
	if err != nil || string(got) != value {
		t.Fatalf("get() = %q, %v", got, err)
	}

	time.Sleep(500 * time.Millisecond)
	if v, err := getField(c, key, ""); v != nil || err != nil {
		t.Fatalf("get() after expiry = %q, %v, want no value", v, err)
	}
}

// An entry and its body: read in one round trip, the body written as it is.
func TestRedisIntegrationEntryAndBody(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)
	body := strings.Repeat("binary\x00\r\nbody", 8<<10) // larger than largeArgBytes

	if err := c.set(ctx, key, fieldValues("f1", "entry", "f1:body", body, "f2", "", "f2:body", "small"), 5*time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	values, err := c.get(ctx, key, []string{"", "f1", "f1:body", "f2", "f2:body"})
	if err != nil || len(values) != 5 {
		t.Fatalf("get() = %d values, %v", len(values), err)
	}
	if values[0] != nil || string(values[1]) != "entry" || string(values[2]) != body {
		t.Errorf("get() = %q, %q and a body of %d bytes", values[0], values[1], len(values[2]))
	}
	if values[3] == nil || len(values[3]) != 0 || string(values[4]) != "small" {
		t.Errorf("get() = %q, %q: an empty field is not a missing one", values[3], values[4])
	}

	// A value larger than what the client accepts is not one of ours: a miss.
	small := newRedisClient(c.opts, 1024)
	if values, err := small.get(ctx, key, []string{"f1", "f1:body"}); err != nil || values[0] != nil || values[1] != nil {
		t.Errorf("get() of an oversized value = %q, %v, want no value", values, err)
	}
	if v, err := getField(small, key, "f1"); err != nil || string(v) != "entry" {
		t.Errorf("get() after an oversized value = %q, %v", v, err)
	}
	if err := c.invalidate(ctx, key); err != nil {
		t.Fatalf("invalidate() = %v", err)
	}
}

func TestRedisIntegrationDel(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)

	if err := c.del(ctx, key, []string{"", ":body"}); err != nil {
		t.Fatalf("del() of missing fields = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("", "entry", ":body", "body", "f1", "other"), time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.del(ctx, key, []string{"", ":body"}); err != nil {
		t.Fatalf("del() = %v", err)
	}
	values, err := c.get(ctx, key, []string{"", ":body", "f1"})
	if err != nil || values[0] != nil || values[1] != nil || string(values[2]) != "other" {
		t.Fatalf("get() after del = %q, %v, want the other field only", values, err)
	}
}

// What a 304 does to a stored response: its entry is written again, and its
// body is kept for as long.
func TestRedisIntegrationExpire(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)

	if err := c.expire(ctx, key, "f1:body", time.Second); err != nil {
		t.Fatalf("expire() of a missing field = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("f1", "entry", "f1:body", "body", "f2:body", "other"), 300*time.Millisecond, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("f1", "entry2"), 5*time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.expire(ctx, key, "f1:body", 5*time.Second); err != nil {
		t.Fatalf("expire() = %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	values, err := c.get(ctx, key, []string{"f1", "f1:body", "f2:body"})
	if err != nil || string(values[0]) != "entry2" || string(values[1]) != "body" || values[2] != nil {
		t.Errorf("get() = %q, %v, want the entry and its body, not the field left to expire", values, err)
	}
	if err := c.invalidate(ctx, key); err != nil {
		t.Fatalf("invalidate() = %v", err)
	}
}

func TestRedisIntegrationVariants(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)

	if n, err := c.count(ctx, key); err != nil || n != 0 {
		t.Fatalf("count() of a new key = %d, %v", n, err)
	}
	// Replacing only: nothing is stored unless the marker and the variant exist.
	if err := c.set(ctx, key, fieldValues("", "m1", "f1", "v1"), time.Second, true); err != nil {
		t.Fatalf("set() replacing = %v", err)
	}
	if n, err := c.count(ctx, key); err != nil || n != 0 {
		t.Fatalf("count() = %d, %v: replacing must not create fields", n, err)
	}

	if err := c.set(ctx, key, fieldValues("", "m1", "f1", "v1"), 300*time.Millisecond, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("", "m2", "f2", "v2"), 5*time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if n, err := c.count(ctx, key); err != nil || n != 3 {
		t.Fatalf("count() = %d, %v, want the marker and 2 variants", n, err)
	}
	if err := c.set(ctx, key, fieldValues("", "m3", "f3", "v3"), time.Second, true); err != nil {
		t.Fatalf("set() replacing = %v", err)
	}
	if got, err := getField(c, key, ""); err != nil || string(got) != "m2" {
		t.Fatalf("marker = %q, %v: replacing a missing variant must not write the marker", got, err)
	}
	if err := c.set(ctx, key, fieldValues("", "m2b", "f2", "v2b"), 5*time.Second, true); err != nil {
		t.Fatalf("set() replacing = %v", err)
	}
	for field, want := range map[string]string{"": "m2b", "f1": "v1", "f2": "v2b"} {
		if got, err := getField(c, key, field); err != nil || string(got) != want {
			t.Errorf("get(%q) = %q, %v, want %q", field, got, err, want)
		}
	}

	// Each field has its own TTL: a variant expires without the others.
	time.Sleep(500 * time.Millisecond)
	if v, err := getField(c, key, "f1"); v != nil || err != nil {
		t.Errorf("get() of an expired variant = %q, %v, want no value", v, err)
	}
	if got, err := getField(c, key, "f2"); err != nil || string(got) != "v2b" {
		t.Errorf("get() = %q, %v", got, err)
	}
	if err := c.del(ctx, key, []string{"", "f2"}); err != nil {
		t.Fatalf("del() = %v", err)
	}
	if n, err := c.count(ctx, key); err != nil || n != 0 {
		t.Errorf("count() = %d, %v after cleanup", n, err)
	}
}

func TestRedisIntegrationInvalidate(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	key := testKey(t)

	if err := c.invalidate(ctx, key); err != nil {
		t.Fatalf("invalidate() of a missing key = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("", "entry", ":body", "body"), 5*time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.set(ctx, key, fieldValues("", "marker", "f1", "v1", "f1:body", "body"), 5*time.Second, false); err != nil {
		t.Fatalf("set() = %v", err)
	}
	if err := c.invalidate(ctx, key); err != nil {
		t.Fatalf("invalidate() = %v", err)
	}
	if n, err := c.count(ctx, key); err != nil || n != 0 {
		t.Errorf("count() = %d, %v: every field must be gone", n, err)
	}
}
