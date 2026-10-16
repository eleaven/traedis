// Package traedis is a Redis-backed HTTP cache middleware plugin for Traefik.
package traedis

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultRedisPort = "6379"

// maxValueBytes is the largest value Redis takes by default (proto-max-bulk-len).
const maxValueBytes = 512 << 20

// Config is the plugin configuration, as written in the Traefik dynamic configuration.
type Config struct {
	Redis                       RedisConfig `json:"redis,omitempty"`
	StatusCodes                 []int       `json:"statusCodes,omitempty"`
	DefaultTTL                  string      `json:"defaultTtl,omitempty"`
	StaleTTL                    string      `json:"staleTtl,omitempty"`
	DefaultStaleWhileRevalidate string      `json:"defaultStaleWhileRevalidate,omitempty"`
	DefaultStaleIfError         string      `json:"defaultStaleIfError,omitempty"`
	MaxBodyBytes                int64       `json:"maxBodyBytes,omitempty"`
	MaxBufferedBytes            int64       `json:"maxBufferedBytes,omitempty"`
	MaxVariants                 int         `json:"maxVariants,omitempty"`
	MaxRevalidations            int         `json:"maxRevalidations,omitempty"`
	RevalidationTimeout         string      `json:"revalidationTimeout,omitempty"`
	SortQuery                   bool        `json:"sortQuery,omitempty"`
	ExposeKey                   bool        `json:"exposeKey,omitempty"`
}

// RedisConfig holds the Redis connection options.
type RedisConfig struct {
	DSN         string `json:"dsn,omitempty"`
	PoolSize    int    `json:"poolSize,omitempty"`
	DialTimeout string `json:"dialTimeout,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	MaxFailures int    `json:"maxFailures,omitempty"`
	RetryAfter  string `json:"retryAfter,omitempty"`
	// TLS only applies to a rediss:// DSN.
	TLS RedisTLSConfig `json:"tls,omitempty"`
}

// RedisTLSConfig tells how the certificate of Redis is verified.
type RedisTLSConfig struct {
	// CA is a PEM file, or its content: the authorities that may have signed
	// the certificate, in place of those of the system.
	CA                 string `json:"ca,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
}

// CreateConfig returns the default configuration (documented in the README).
func CreateConfig() *Config {
	return &Config{
		Redis: RedisConfig{
			DSN:         "redis://cache:6379/0",
			PoolSize:    10,
			DialTimeout: "50ms",
			Timeout:     "1s",
			MaxFailures: 5,
			RetryAfter:  "1s",
		},
		StatusCodes:                 []int{200},
		DefaultTTL:                  "5m",
		StaleTTL:                    "1h",
		DefaultStaleWhileRevalidate: "0s",
		DefaultStaleIfError:         "0s",
		MaxBodyBytes:                5 << 20,
		MaxBufferedBytes:            64 << 20,
		MaxVariants:                 16,
		MaxRevalidations:            10,
		RevalidationTimeout:         "30s",
		SortQuery:                   false,
		ExposeKey:                   false,
	}
}

// settings is the validated, parsed form of Config.
type settings struct {
	redis                       redisOptions
	statusCodes                 []int
	defaultTTL                  time.Duration
	staleTTL                    time.Duration
	defaultStaleWhileRevalidate time.Duration
	defaultStaleIfError         time.Duration
	maxBodyBytes                int64
	// maxBufferedBytes bounds the bodies held in memory at once (see budget).
	maxBufferedBytes int64
	// maxVariants is the number of variants a URI key may hold, on top of its
	// marker. Past it, new variants are not stored: stored ones are never
	// evicted. 0: responses with Vary are not stored.
	maxVariants int
	// Background revalidations (RFC 5861 §3) are bounded: maxRevalidations at
	// once, each given revalidationTimeout (see revalidate).
	maxRevalidations    int
	revalidationTimeout time.Duration
	// sortQuery makes the order of the query parameters irrelevant to the
	// cache key (see cacheURI).
	sortQuery bool
	exposeKey bool
}

// redisOptions describes how to reach Redis. It holds credentials: never log it.
type redisOptions struct {
	addr     string
	username string
	password string
	db       int
	// tls is set for a rediss:// DSN: connections are encrypted, and the
	// certificate of Redis verified with it.
	tls *tls.Config
	// poolSize is the number of connections, at most.
	poolSize int
	// dialTimeout bounds getting a connection, a free one of the pool or a new
	// one; timeout bounds a whole operation, getting its connection included.
	dialTimeout time.Duration
	timeout     time.Duration
	// After maxFailures failures in a row, Redis is only tried again every
	// retryAfter (see do). 0: it is tried by every command.
	maxFailures int
	retryAfter  time.Duration
}

func parseConfig(cfg *Config) (settings, error) {
	var s settings
	if cfg == nil {
		return s, errors.New("missing configuration")
	}

	redis, err := parseDSN(cfg.Redis.DSN)
	if err != nil {
		return s, err
	}
	if err := setTLS(&redis, cfg.Redis.TLS); err != nil {
		return s, err
	}
	if redis.dialTimeout, err = parseDuration("redis.dialTimeout", cfg.Redis.DialTimeout); err != nil {
		return s, err
	}
	if redis.timeout, err = parseDuration("redis.timeout", cfg.Redis.Timeout); err != nil {
		return s, err
	}
	if redis.dialTimeout <= 0 || redis.timeout <= 0 {
		return s, errors.New("redis.dialTimeout and redis.timeout must be positive")
	}
	// An operation starts by getting its connection.
	if redis.dialTimeout > redis.timeout {
		return s, errors.New("redis.dialTimeout must not exceed redis.timeout")
	}
	if cfg.Redis.PoolSize <= 0 {
		return s, errors.New("redis.poolSize must be positive")
	}
	redis.poolSize = cfg.Redis.PoolSize
	if cfg.Redis.MaxFailures < 0 {
		return s, errors.New("redis.maxFailures must not be negative")
	}
	redis.maxFailures = cfg.Redis.MaxFailures
	if redis.retryAfter, err = parseDuration("redis.retryAfter", cfg.Redis.RetryAfter); err != nil {
		return s, err
	}
	if redis.retryAfter <= 0 {
		return s, errors.New("redis.retryAfter must be positive")
	}
	s.redis = redis

	if s.defaultTTL, err = parseDuration("defaultTtl", cfg.DefaultTTL); err != nil {
		return s, err
	}
	if s.staleTTL, err = parseDuration("staleTtl", cfg.StaleTTL); err != nil {
		return s, err
	}
	if s.defaultStaleWhileRevalidate, err = parseDuration("defaultStaleWhileRevalidate", cfg.DefaultStaleWhileRevalidate); err != nil {
		return s, err
	}
	if s.defaultStaleIfError, err = parseDuration("defaultStaleIfError", cfg.DefaultStaleIfError); err != nil {
		return s, err
	}

	for _, window := range []struct {
		name  string
		value time.Duration
	}{
		{"defaultStaleWhileRevalidate", s.defaultStaleWhileRevalidate},
		{"defaultStaleIfError", s.defaultStaleIfError},
	} {
		// Nothing is kept past staleTtl: a longer window would never be honoured.
		if window.value > s.staleTTL {
			return s, fmt.Errorf("%s must not exceed staleTtl", window.name)
		}
	}

	if len(cfg.StatusCodes) == 0 {
		return s, errors.New("statusCodes must not be empty")
	}
	for _, code := range cfg.StatusCodes {
		if code < 100 || code > 599 {
			return s, fmt.Errorf("statusCodes: invalid status code %d", code)
		}
		// See storeTTL: these are never stored, whatever the configuration.
		if code < 200 || code == http.StatusPartialContent || code == http.StatusNotModified {
			return s, fmt.Errorf("statusCodes: a %d response is never stored", code)
		}
	}
	s.statusCodes = append([]int(nil), cfg.StatusCodes...)

	if cfg.MaxBodyBytes <= 0 {
		return s, errors.New("maxBodyBytes must be positive")
	}
	if cfg.MaxBodyBytes > maxValueBytes {
		return s, fmt.Errorf("maxBodyBytes must not exceed %d, the largest value Redis takes", maxValueBytes)
	}
	s.maxBodyBytes = cfg.MaxBodyBytes

	// Any response that may be stored has to fit.
	if cfg.MaxBufferedBytes < cfg.MaxBodyBytes {
		return s, errors.New("maxBufferedBytes must not be under maxBodyBytes")
	}
	s.maxBufferedBytes = cfg.MaxBufferedBytes

	if cfg.MaxVariants < 0 {
		return s, errors.New("maxVariants must not be negative")
	}
	s.maxVariants = cfg.MaxVariants

	if cfg.MaxRevalidations <= 0 {
		return s, errors.New("maxRevalidations must be positive")
	}
	s.maxRevalidations = cfg.MaxRevalidations
	if s.revalidationTimeout, err = parseDuration("revalidationTimeout", cfg.RevalidationTimeout); err != nil {
		return s, err
	}
	if s.revalidationTimeout <= 0 {
		return s, errors.New("revalidationTimeout must be positive")
	}

	s.sortQuery = cfg.SortQuery
	s.exposeKey = cfg.ExposeKey

	return s, nil
}

func parseDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", name, value)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: negative duration %q", name, value)
	}
	return d, nil
}

// setTLS applies redis.tls to the options of a rediss:// DSN.
func setTLS(o *redisOptions, cfg RedisTLSConfig) error {
	if o.tls == nil {
		if cfg.CA != "" || cfg.InsecureSkipVerify {
			return errors.New("redis.tls: the scheme of redis.dsn must be rediss://")
		}
		return nil
	}
	o.tls.InsecureSkipVerify = cfg.InsecureSkipVerify
	if cfg.CA == "" {
		return nil
	}
	pem := []byte(cfg.CA)
	if !strings.Contains(cfg.CA, "-----BEGIN") {
		var err error
		if pem, err = os.ReadFile(cfg.CA); err != nil {
			return fmt.Errorf("redis.tls.ca: %w", err)
		}
	}
	o.tls.RootCAs = x509.NewCertPool()
	if !o.tls.RootCAs.AppendCertsFromPEM(pem) {
		return errors.New("redis.tls.ca: no certificate found")
	}
	return nil
}

// parseDSN parses redis://[user[:password]@]host[:port][/db], or rediss:// for
// TLS. Errors never include the DSN itself, which may hold credentials.
func parseDSN(dsn string) (redisOptions, error) {
	var o redisOptions
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return o, errors.New("redis.dsn: malformed URL")
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return o, errors.New("redis.dsn: scheme must be redis:// or rediss://")
	}
	if u.Hostname() == "" {
		return o, errors.New("redis.dsn: missing host")
	}
	if u.Scheme == "rediss" {
		o.tls = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	}
	port := u.Port()
	if port == "" {
		port = defaultRedisPort
	}
	o.addr = net.JoinHostPort(u.Hostname(), port)

	if u.User != nil {
		o.username = u.User.Username()
		o.password, _ = u.User.Password()
	}

	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		n, err := strconv.Atoi(db)
		if err != nil || n < 0 {
			return o, errors.New("redis.dsn: database must be a non-negative integer")
		}
		o.db = n
	}
	return o, nil
}
