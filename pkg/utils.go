package traedis

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// unquote strips the double quotes around a quoted-string (RFC 9110 §5.6.4). Any other value is returned unchanged; escaped characters are
// not decoded.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// seconds converts a delta-seconds value (RFC 9111 §1.2.2) to a duration.
func seconds(n int64) time.Duration {
	return time.Duration(n) * time.Second
}

// hashHex returns the lowercase hex of the first 16 bytes of the SHA-256 of s. Redis keys and variant fields are made of it: changing it
// invalidates the whole cache.
func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// sameStrings reports whether a and b hold the same strings in the same order (slices.Equal does not run under Yaegi v0.16.1).
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
