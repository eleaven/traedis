package traedis

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleEntry() *entry {
	return &entry{
		status: 200,
		header: http.Header{
			"Content-Type":  {"image/png"},
			"Cache-Control": {"public, max-age=60"},
			"X-Multi":       {"a", "b"},
			"X-Empty":       {""},
		},
		size:         9,
		requestTime:  time.Unix(1700000000, 123),
		responseTime: time.Unix(1700000001, 456),
		initialAge:   3 * time.Second,
		lifetime:     time.Minute,
		vary:         http.Header{"Accept-Language": {"fr"}, "X-Absent-Value": {""}},
	}
}

func TestEntryRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		e    *entry
	}{
		{name: "full entry", e: sampleEntry()},
		{name: "empty body and headers, no explicit freshness", e: &entry{status: 204, header: http.Header{}, vary: http.Header{}, lifetime: -1, requestTime: time.Unix(0, 0), responseTime: time.Unix(0, 0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := encodeEntry(tt.e)
			got, err := decodeEntry(data, 1<<20)
			if err != nil {
				t.Fatalf("decodeEntry() = %v", err)
			}
			if got.status != tt.e.status || got.size != tt.e.size || got.body != nil {
				t.Errorf("status = %d, size = %d, body = %q: the body is not part of the entry", got.status, got.size, got.body)
			}
			if !got.requestTime.Equal(tt.e.requestTime) || !got.responseTime.Equal(tt.e.responseTime) {
				t.Errorf("times = %v %v", got.requestTime, got.responseTime)
			}
			if got.initialAge != tt.e.initialAge || got.lifetime != tt.e.lifetime {
				t.Errorf("initialAge = %v, lifetime = %v", got.initialAge, got.lifetime)
			}
			if !reflect.DeepEqual(got.header, tt.e.header) || !reflect.DeepEqual(got.vary, tt.e.vary) {
				t.Errorf("header = %v, vary = %v", got.header, got.vary)
			}
			if !bytes.Equal(got.raw, data) {
				t.Error("the entry must keep what it was decoded from")
			}
		})
	}
}

// The fields are written as HTTP does: what it would not send is not stored.
func TestEntryFieldsAreHTTPFields(t *testing.T) {
	e := sampleEntry()
	e.header = http.Header{"x-lower": {"a"}, "X-Spaces": {"  b  "}, "X-Lines": {"c\r\nX-Injected: d"}, "Bad Name": {"e"}, "X-None": nil}
	got, err := decodeEntry(encodeEntry(e), 1<<20)
	if err != nil {
		t.Fatalf("decodeEntry() = %v", err)
	}
	want := http.Header{"X-Lower": {"a"}, "X-Spaces": {"b"}, "X-Lines": {"c  X-Injected: d"}}
	if !reflect.DeepEqual(got.header, want) {
		t.Errorf("header = %q, want %q", got.header, want)
	}
}

func TestDecodeEntryRejectsInvalidData(t *testing.T) {
	valid := encodeEntry(sampleEntry())
	with := func(offset int, value uint64) []byte {
		b := append([]byte(nil), valid...)
		binary.BigEndian.PutUint64(b[offset:], value)
		return b
	}
	fields := func(s string) []byte {
		return append(append([]byte(nil), valid[:entryFixed]...), s...)
	}
	negative := uint64(1) << 63

	tests := []struct {
		name    string
		data    []byte
		maxBody int64
	}{
		{name: "empty", data: nil, maxBody: 1 << 20},
		{name: "unknown version is a miss", data: append([]byte("TRD9"), valid[4:]...), maxBody: 1 << 20},
		{name: "previous version is a miss", data: append([]byte("TRD1"), valid[4:]...), maxBody: 1 << 20},
		{name: "body larger than maxBodyBytes is a miss", data: valid, maxBody: sampleEntry().size - 1},
		{name: "oversized entry is a miss", data: fields("X-Large: " + strings.Repeat("a", entryOverhead) + "\r\n\r\n\r\n"), maxBody: 1 << 20},
		{name: "trailing garbage", data: append(append([]byte(nil), valid...), 0), maxBody: 1 << 20},
		{name: "negative size", data: with(38, negative), maxBody: 1 << 20},
		{name: "negative initial age", data: with(22, negative), maxBody: 1 << 20},
		{name: "lifetime below -1", data: with(30, negative), maxBody: 1 << 20},
		{name: "no fields", data: fields(""), maxBody: 1 << 20},
		{name: "no vary fields", data: fields("X-A: b\r\n\r\n"), maxBody: 1 << 20},
		{name: "field line without a name", data: fields(": b\r\n\r\n\r\n"), maxBody: 1 << 20},
		{name: "field line without a colon", data: fields("X-A\r\n\r\n\r\n"), maxBody: 1 << 20},
		{name: "fields starting with a continuation line", data: fields(" X-A: b\r\n\r\n\r\n"), maxBody: 1 << 20},
		{name: "invalid status", data: func() []byte {
			b := append([]byte(nil), valid...)
			binary.BigEndian.PutUint16(b[4:], 42)
			return b
		}(), maxBody: 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeEntry(tt.data, tt.maxBody); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := decodeEntry(fields("X-A: b\r\n\r\n\r\n"), 1<<20); err != nil {
		t.Errorf("the fields these cases are made from must be valid: %v", err)
	}
}

func TestDecodeEntryRejectsEveryTruncation(t *testing.T) {
	valid := encodeEntry(sampleEntry())
	for n := 0; n < len(valid); n++ {
		if _, err := decodeEntry(valid[:n], 1<<20); err == nil {
			t.Fatalf("truncated at %d bytes: expected an error", n)
		}
	}
}
