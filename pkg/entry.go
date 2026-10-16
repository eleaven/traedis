package traedis

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
	"net/textproto"
	"time"
)

// entryVersion prefixes every stored entry. Bump it on any format change:
// entries with an unknown version are treated as a miss.
const entryVersion = "TRD2"

// entryFixed is the size of an encoded entry without its fields.
const entryFixed = len(entryVersion) + 2 + 5*8

var errBadEntry = errors.New("invalid cache entry")

// entry is a stored response. Its body is stored apart, in a field of its own
// (see bodyField): a 304 and the answer to a HEAD are made without reading it.
//
// Encoding (big endian):
//
//	"TRD2" | status u16 | requestTime i64 | responseTime i64 (Unix nanoseconds)
//	| initialAge i64 | lifetime i64 (nanoseconds) | size u64
//	| header fields | vary fields
//
// where fields are the lines of an HTTP header ("Name: value\r\n") and an
// empty one. The standard library writes and parses them: under Yaegi, that is
// many times faster than code of ours.
type entry struct {
	status int
	header http.Header
	// body is nil until it is read from the store (see find and ready); size
	// is its length either way.
	body         []byte
	size         int64
	requestTime  time.Time
	responseTime time.Time
	// Worked out once, when the response is received (see setFreshness), rather
	// than on every hit: initialAge is its corrected initial age (§4.2.3) and
	// lifetime its explicit freshness lifetime (§4.2.1), -1 without one.
	initialAge time.Duration
	lifetime   time.Duration
	// vary holds the request header values selected by the response Vary; for
	// Accept-Encoding, the content codings the request accepted (see vary.go).
	vary http.Header
	// raw is the entry as it was read from the store, if it was: what tells
	// whether a body read later is still its own (see ready).
	raw []byte
	// held is what its body holds of the memory budget (see holdBody).
	held int64
}

// bodyField returns the field holding the body of the entry of field.
func bodyField(field string) string {
	return field + ":body"
}

func encodeEntry(e *entry) []byte {
	fixed := make([]byte, 0, entryFixed)
	fixed = append(fixed, entryVersion...)
	fixed = binary.BigEndian.AppendUint16(fixed, uint16(e.status))
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(e.requestTime.UnixNano()))
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(e.responseTime.UnixNano()))
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(e.initialAge))
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(e.lifetime))
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(e.size))

	var b bytes.Buffer
	b.Write(fixed)
	// Writing to a buffer never fails.
	_ = e.header.Write(&b)
	b.WriteString("\r\n")
	_ = e.vary.Write(&b)
	b.WriteString("\r\n")
	return b.Bytes()
}

// decodeEntry parses an entry, without its body. Stored data is untrusted: any
// inconsistency is an error, and so is a body larger than maxBody.
func decodeEntry(b []byte, maxBody int64) (*entry, error) {
	if len(b) > entryOverhead || len(b) < entryFixed || string(b[:len(entryVersion)]) != entryVersion {
		return nil, errBadEntry
	}
	e := &entry{raw: b}
	e.status = int(binary.BigEndian.Uint16(b[4:]))
	e.requestTime = time.Unix(0, int64(binary.BigEndian.Uint64(b[6:])))
	e.responseTime = time.Unix(0, int64(binary.BigEndian.Uint64(b[14:])))
	e.initialAge = time.Duration(binary.BigEndian.Uint64(b[22:]))
	e.lifetime = time.Duration(binary.BigEndian.Uint64(b[30:]))
	e.size = int64(binary.BigEndian.Uint64(b[38:]))
	if e.status < 100 || e.status > 599 || e.initialAge < 0 || e.lifetime < -1 || e.size < 0 || e.size > maxBody {
		return nil, errBadEntry
	}

	fields := bytes.NewReader(b[entryFixed:])
	buffered := bufio.NewReaderSize(fields, len(b))
	r := textproto.NewReader(buffered)
	header, err := r.ReadMIMEHeader()
	if err != nil {
		return nil, errBadEntry
	}
	vary, err := r.ReadMIMEHeader()
	if err != nil || buffered.Buffered() != 0 || fields.Len() != 0 {
		return nil, errBadEntry
	}
	e.header = http.Header(header)
	e.vary = http.Header(vary)
	return e, nil
}
