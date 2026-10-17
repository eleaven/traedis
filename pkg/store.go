package traedis

import (
	"context"
	"time"
)

// store keeps stored responses in one hash per URI key. A response takes two fields, its entry and its body (see bodyField): one per
// variant, on top of the marker of the key, when its responses vary.
type store interface {
	// get returns the values of fields in key, in the same order: nil for a field that is not there.
	get(ctx context.Context, key string, fields []string) ([][]byte, error)
	// set stores fields in key, at once, all expiring after ttl. With replace, nothing is stored unless all of them already exist.
	set(ctx context.Context, key string, fields []fieldValue, ttl time.Duration, replace bool) error
	// expire makes field of key expire after ttl, if it is there.
	expire(ctx context.Context, key, field string, ttl time.Duration) error
	// del removes fields from key.
	del(ctx context.Context, key string, fields []string) error
	// count returns the number of fields of key.
	count(ctx context.Context, key string) (int, error)
	// invalidate removes key: its stored responses and its marker.
	invalidate(ctx context.Context, key string) error
}

// fieldValue is a field of a key and the value to store in it.
type fieldValue struct {
	name  string
	value []byte
}
