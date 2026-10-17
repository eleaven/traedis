package traedis

import (
	"context"
	"sync"
	"time"
)

// memStore is the in-memory store used by middleware tests.
type memStore struct {
	mu   sync.Mutex
	data map[string]map[string]memValue
	err  error // returned by every call when set
	// variantErr is returned when reading a field other than "" and its body.
	variantErr error
	reads      [][]string // fields asked for by each read: one per round trip
	sets       int
	dels       int
	// invalidations counts the keys removed as a whole.
	invalidations int
}

type memValue struct {
	value []byte
	ttl   time.Duration
}

func newMemStore() *memStore {
	return &memStore{data: map[string]map[string]memValue{}}
}

func (m *memStore) get(_ context.Context, key string, fields []string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = append(m.reads, append([]string(nil), fields...))
	if m.err != nil {
		return nil, m.err
	}
	values := make([][]byte, len(fields))
	for i, field := range fields {
		if m.variantErr != nil && field != "" && field != bodyField("") {
			return nil, m.variantErr
		}
		if v, ok := m.data[key][field]; ok {
			values[i] = v.value
		}
	}
	return values, nil
}

func (m *memStore) set(_ context.Context, key string, fields []fieldValue, ttl time.Duration, replace bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	for _, f := range fields {
		if _, ok := m.data[key][f.name]; replace && !ok {
			return nil
		}
	}
	if m.data[key] == nil {
		m.data[key] = map[string]memValue{}
	}
	for _, f := range fields {
		// Never nil: that is a field that is not there.
		m.data[key][f.name] = memValue{value: append([]byte{}, f.value...), ttl: ttl}
	}
	m.sets++
	return nil
}

func (m *memStore) expire(_ context.Context, key, field string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if v, ok := m.data[key][field]; ok {
		m.data[key][field] = memValue{value: v.value, ttl: ttl}
	}
	return nil
}

func (m *memStore) count(_ context.Context, key string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	return len(m.data[key]), nil
}

func (m *memStore) del(_ context.Context, key string, fields []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	for _, field := range fields {
		delete(m.data[key], field)
	}
	m.dels++
	return nil
}

func (m *memStore) invalidate(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	delete(m.data, key)
	m.invalidations++
	return nil
}

func (m *memStore) lookup(key, field string) (memValue, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key][field]
	return v, ok
}

// put stores e in field of key the way the cache does, in one write: its entry and its body, with the marker of the key when it is given
// one. The size and the freshness of e are worked out from its body and its header.
func (m *memStore) put(key, field string, e *entry, marker []byte, ttl time.Duration) error {
	cc := parseCacheControl(e.header.Values("Cache-Control"))
	stored := &entry{
		status:       e.status,
		header:       e.header,
		size:         int64(len(e.body)),
		requestTime:  e.requestTime,
		responseTime: e.responseTime,
		vary:         e.vary,
	}
	setFreshness(stored, cc)
	var fields []fieldValue
	if marker != nil {
		fields = append(fields, fieldValue{name: "", value: marker})
	}
	fields = append(fields, fieldValue{name: field, value: encodeEntry(stored)}, fieldValue{name: bodyField(field), value: e.body})
	return m.set(context.Background(), key, fields, ttl, false)
}

// putRaw stores value in field of key.
func (m *memStore) putRaw(key, field string, value []byte) error {
	return m.set(context.Background(), key, []fieldValue{{name: field, value: value}}, time.Hour, false)
}

// entry returns the entry stored in field of key, with its body, or nil.
func (m *memStore) entry(key, field string) *entry {
	v, ok := m.lookup(key, field)
	if !ok {
		return nil
	}
	e, err := decodeEntry(v.value, 1<<30)
	if err != nil {
		return nil
	}
	body, _ := m.lookup(key, bodyField(field))
	e.body = body.value
	return e
}
