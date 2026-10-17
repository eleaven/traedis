package traedis

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCreateConfigDefaults(t *testing.T) {
	s, err := parseConfig(CreateConfig())
	if err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if s.redis.addr != "cache:6379" || s.redis.db != 0 || s.redis.dialTimeout != 50*time.Millisecond || s.redis.timeout != time.Second ||
		s.redis.poolSize != 10 || s.redis.maxFailures != 5 || s.redis.retryAfter != time.Second {
		t.Errorf("redis = %+v", s.redis)
	}
	// 200-399, without 206 and 304: no error is stored.
	if n := len(s.statusCodes); n != 198 || s.statusCodes[0] != 200 || s.statusCodes[n-1] != 399 {
		t.Errorf("statusCodes = %v", s.statusCodes)
	}
	if s.defaultTTL != 5*time.Minute || s.staleTTL != time.Hour {
		t.Errorf("defaultTTL = %v, staleTTL = %v", s.defaultTTL, s.staleTTL)
	}
	if s.defaultStaleWhileRevalidate != 0 || s.defaultStaleIfError != 0 {
		t.Errorf("§4.2.4 no stale response unless the backend or the configuration allows it: %v, %v", s.defaultStaleWhileRevalidate, s.defaultStaleIfError)
	}
	if s.maxRevalidations != 10 || s.revalidationTimeout != 30*time.Second {
		t.Errorf("maxRevalidations = %d, revalidationTimeout = %v", s.maxRevalidations, s.revalidationTimeout)
	}
	if len(s.authorizationHeaders) != 0 || !s.ignoreClientCacheControl {
		t.Errorf("authorizationHeaders = %v, ignoreClientCacheControl = %v", s.authorizationHeaders, s.ignoreClientCacheControl)
	}
	if s.maxBodyBytes != 5242880 || s.maxBufferedBytes != 64<<20 || s.maxVariants != 16 || s.sortQuery || s.exposeKey {
		t.Errorf("settings = %+v", s)
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
		check   func(t *testing.T, s settings)
	}{
		{name: "invalid redis.timeout", mutate: func(c *Config) { c.Redis.Timeout = "fast" }, wantErr: "redis.timeout"},
		{name: "zero redis.timeout", mutate: func(c *Config) { c.Redis.Timeout = "0" }, wantErr: "redis.timeout"},
		{name: "invalid redis.dialTimeout", mutate: func(c *Config) { c.Redis.DialTimeout = "fast" }, wantErr: "redis.dialTimeout"},
		{name: "zero redis.dialTimeout", mutate: func(c *Config) { c.Redis.DialTimeout = "0" }, wantErr: "redis.dialTimeout"},
		{name: "redis.dialTimeout past redis.timeout", mutate: func(c *Config) { c.Redis.Timeout = "20ms" }, wantErr: "redis.dialTimeout must not exceed redis.timeout"},
		{name: "zero redis.poolSize", mutate: func(c *Config) { c.Redis.PoolSize = 0 }, wantErr: "redis.poolSize"},
		{name: "zero maxRevalidations", mutate: func(c *Config) { c.MaxRevalidations = 0 }, wantErr: "maxRevalidations"},
		{name: "invalid revalidationTimeout", mutate: func(c *Config) { c.RevalidationTimeout = "30" }, wantErr: "revalidationTimeout"},
		{name: "zero revalidationTimeout", mutate: func(c *Config) { c.RevalidationTimeout = "0s" }, wantErr: "revalidationTimeout"},
		{name: "negative redis.maxFailures", mutate: func(c *Config) { c.Redis.MaxFailures = -1 }, wantErr: "redis.maxFailures"},
		{
			name:   "redis.maxFailures 0 never gives up on Redis",
			mutate: func(c *Config) { c.Redis.MaxFailures = 0 },
			check: func(t *testing.T, s settings) {
				if s.redis.maxFailures != 0 {
					t.Errorf("maxFailures = %d", s.redis.maxFailures)
				}
			},
		},
		{name: "invalid redis.retryAfter", mutate: func(c *Config) { c.Redis.RetryAfter = "soon" }, wantErr: "redis.retryAfter"},
		{name: "zero redis.retryAfter", mutate: func(c *Config) { c.Redis.RetryAfter = "0" }, wantErr: "redis.retryAfter"},
		{name: "invalid defaultTtl", mutate: func(c *Config) { c.DefaultTTL = "5 minutes" }, wantErr: "defaultTtl"},
		{name: "negative staleTtl", mutate: func(c *Config) { c.StaleTTL = "-1s" }, wantErr: "staleTtl"},
		{name: "invalid defaultStaleWhileRevalidate", mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "30" }, wantErr: "defaultStaleWhileRevalidate"},
		{name: "negative defaultStaleIfError", mutate: func(c *Config) { c.DefaultStaleIfError = "-1s" }, wantErr: "defaultStaleIfError"},
		{
			name:   "defaultTtl 0 disables heuristic freshness",
			mutate: func(c *Config) { c.DefaultTTL = "0" },
			check: func(t *testing.T, s settings) {
				if s.defaultTTL != 0 {
					t.Errorf("defaultTTL = %v", s.defaultTTL)
				}
			},
		},
		{name: "defaultStaleWhileRevalidate past staleTtl", mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "2h" }, wantErr: "defaultStaleWhileRevalidate must not exceed staleTtl"},
		{name: "defaultStaleIfError without staleTtl", mutate: func(c *Config) { c.StaleTTL = "0"; c.DefaultStaleIfError = "10m" }, wantErr: "defaultStaleIfError must not exceed staleTtl"},
		{
			name:   "stale windows up to staleTtl",
			mutate: func(c *Config) { c.DefaultStaleWhileRevalidate = "1h"; c.DefaultStaleIfError = "1h" },
			check: func(t *testing.T, s settings) {
				if s.defaultStaleWhileRevalidate != time.Hour || s.defaultStaleIfError != time.Hour {
					t.Errorf("settings = %+v", s)
				}
			},
		},
		{name: "invalid status code", mutate: func(c *Config) { c.StatusCodes = []string{"200", "999"} }, wantErr: `statusCodes: invalid status code "999"`},
		{name: "status code that is not a number", mutate: func(c *Config) { c.StatusCodes = []string{"2xx"} }, wantErr: `statusCodes: invalid status code "2xx"`},
		{name: "no status code", mutate: func(c *Config) { c.StatusCodes = []string{} }, wantErr: "statusCodes must not be empty"},
		{name: "informational status code", mutate: func(c *Config) { c.StatusCodes = []string{"200", "103"} }, wantErr: "a 103 response is never stored"},
		{name: "§3.3 partial content", mutate: func(c *Config) { c.StatusCodes = []string{"200", "206"} }, wantErr: "a 206 response is never stored"},
		{name: "§3.2 not modified", mutate: func(c *Config) { c.StatusCodes = []string{"304"} }, wantErr: "a 304 response is never stored"},
		{name: "range of a single code never stored", mutate: func(c *Config) { c.StatusCodes = []string{"304-304"} }, wantErr: "a 304 response is never stored"},
		{name: "informational range", mutate: func(c *Config) { c.StatusCodes = []string{"100-199"} }, wantErr: "no response of 100-199 is ever stored"},
		{name: "reversed range", mutate: func(c *Config) { c.StatusCodes = []string{"499-400"} }, wantErr: `statusCodes: invalid range "499-400"`},
		{name: "range past 599", mutate: func(c *Config) { c.StatusCodes = []string{"500-600"} }, wantErr: `statusCodes: invalid range "500-600"`},
		{name: "range without an end", mutate: func(c *Config) { c.StatusCodes = []string{"400-"} }, wantErr: `statusCodes: invalid range "400-"`},
		{name: "range without a start", mutate: func(c *Config) { c.StatusCodes = []string{"-499"} }, wantErr: `statusCodes: invalid status code "-499"`},
		{
			name:   "ranges are expanded, without the codes never stored",
			mutate: func(c *Config) { c.StatusCodes = []string{" 404", "300 - 304", "100-210", "404"} },
			check: func(t *testing.T, s settings) {
				want := []int{200, 201, 202, 203, 204, 205, 207, 208, 209, 210, 300, 301, 302, 303, 404}
				if !reflect.DeepEqual(s.statusCodes, want) {
					t.Errorf("statusCodes = %v, want %v", s.statusCodes, want)
				}
			},
		},
		{name: "invalid authorizationHeaders", mutate: func(c *Config) { c.AuthorizationHeaders = []string{"X-User", "X User"} }, wantErr: `authorizationHeaders: invalid header field name "X User"`},
		{name: "empty authorizationHeaders name", mutate: func(c *Config) { c.AuthorizationHeaders = []string{""} }, wantErr: "authorizationHeaders"},
		{
			name:   "authorizationHeaders are canonical, without duplicates",
			mutate: func(c *Config) { c.AuthorizationHeaders = []string{" x-webauth-user", "X-Api-Key", "X-WEBAUTH-USER"} },
			check: func(t *testing.T, s settings) {
				if !reflect.DeepEqual(s.authorizationHeaders, []string{"X-Webauth-User", "X-Api-Key"}) {
					t.Errorf("authorizationHeaders = %q", s.authorizationHeaders)
				}
			},
		},
		{name: "zero maxBodyBytes", mutate: func(c *Config) { c.MaxBodyBytes = 0 }, wantErr: "maxBodyBytes"},
		{name: "maxBodyBytes past what Redis takes", mutate: func(c *Config) { c.MaxBodyBytes = 512<<20 + 1 }, wantErr: "maxBodyBytes must not exceed"},
		{name: "zero maxBufferedBytes", mutate: func(c *Config) { c.MaxBufferedBytes = 0 }, wantErr: "maxBufferedBytes must not be under maxBodyBytes"},
		{name: "maxBufferedBytes under maxBodyBytes", mutate: func(c *Config) { c.MaxBodyBytes = 100 << 20 }, wantErr: "maxBufferedBytes must not be under maxBodyBytes"},
		{
			name:   "maxBufferedBytes as large as maxBodyBytes",
			mutate: func(c *Config) { c.MaxBodyBytes = 100 << 20; c.MaxBufferedBytes = 100 << 20 },
			check: func(t *testing.T, s settings) {
				if s.maxBufferedBytes != 100<<20 {
					t.Errorf("maxBufferedBytes = %d", s.maxBufferedBytes)
				}
			},
		},
		{name: "negative maxVariants", mutate: func(c *Config) { c.MaxVariants = -1 }, wantErr: "maxVariants"},
		{
			name:   "maxVariants 0 disables variants",
			mutate: func(c *Config) { c.MaxVariants = 0 },
			check: func(t *testing.T, s settings) {
				if s.maxVariants != 0 {
					t.Errorf("maxVariants = %d", s.maxVariants)
				}
			},
		},
		{name: "dsn scheme must be redis or rediss", mutate: func(c *Config) { c.Redis.DSN = "http://cache:6379" }, wantErr: "scheme"},
		{
			name:   "redis:// is not encrypted",
			mutate: func(c *Config) { c.Redis.DSN = "redis://cache" },
			check: func(t *testing.T, s settings) {
				if s.redis.tls != nil {
					t.Errorf("tls = %+v", s.redis.tls)
				}
			},
		},
		{
			name:   "rediss:// verifies the certificate with the system authorities",
			mutate: func(c *Config) { c.Redis.DSN = "rediss://user:s3cret@Redis.example/1" },
			check: func(t *testing.T, s settings) {
				o := s.redis
				if o.addr != "Redis.example:6379" || o.password != "s3cret" || o.db != 1 {
					t.Errorf("redis = %+v", o)
				}
				if o.tls == nil || o.tls.ServerName != "Redis.example" || o.tls.RootCAs != nil || o.tls.InsecureSkipVerify {
					t.Errorf("tls = %+v", o.tls)
				}
			},
		},
		{
			name:   "redis.tls.insecureSkipVerify",
			mutate: func(c *Config) { c.Redis.DSN = "rediss://cache"; c.Redis.TLS.InsecureSkipVerify = true },
			check: func(t *testing.T, s settings) {
				if s.redis.tls == nil || !s.redis.tls.InsecureSkipVerify {
					t.Errorf("tls = %+v", s.redis.tls)
				}
			},
		},
		{name: "redis.tls.ca without rediss://", mutate: func(c *Config) { c.Redis.TLS.CA = "/certs/ca.crt" }, wantErr: "redis.tls"},
		{name: "redis.tls.insecureSkipVerify without rediss://", mutate: func(c *Config) { c.Redis.TLS.InsecureSkipVerify = true }, wantErr: "redis.tls"},
		{name: "redis.tls.ca file not found", mutate: func(c *Config) { c.Redis.DSN = "rediss://cache"; c.Redis.TLS.CA = "/no/such/ca.crt" }, wantErr: "redis.tls.ca"},
		{
			name:    "redis.tls.ca without certificate",
			mutate:  func(c *Config) { c.Redis.DSN = "rediss://cache"; c.Redis.TLS.CA = "-----BEGIN CERTIFICATE-----\n" },
			wantErr: "redis.tls.ca: no certificate found",
		},
		{name: "dsn missing host", mutate: func(c *Config) { c.Redis.DSN = "redis:///0" }, wantErr: "host"},
		{name: "dsn invalid database", mutate: func(c *Config) { c.Redis.DSN = "redis://cache/abc" }, wantErr: "database"},
		{
			name:   "dsn with credentials, default port and database",
			mutate: func(c *Config) { c.Redis.DSN = "redis://user:s3cret@Redis.example/15" },
			check: func(t *testing.T, s settings) {
				o := s.redis
				if o.addr != "Redis.example:6379" || o.username != "user" || o.password != "s3cret" || o.db != 15 {
					t.Errorf("redis = %+v", o)
				}
			},
		},
		{
			name:   "dsn with password only",
			mutate: func(c *Config) { c.Redis.DSN = "redis://:s3cret@cache:6380" },
			check: func(t *testing.T, s settings) {
				o := s.redis
				if o.addr != "cache:6380" || o.username != "" || o.password != "s3cret" || o.db != 0 {
					t.Errorf("redis = %+v", o)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CreateConfig()
			tt.mutate(cfg)
			s, err := parseConfig(cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, s)
			}
		})
	}
}

func TestParseDSNErrorsDoNotLeakCredentials(t *testing.T) {
	for _, dsn := range []string{
		"http://user:s3cret@cache",
		"redis://user:s3cret@cache/x",
		"redis://user:s3cret@cache:port",
		"redis://user:s3cret@[::1",
	} {
		_, err := parseDSN(dsn)
		if err == nil {
			t.Errorf("%q: expected an error", dsn)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

// The plugin catalog runs New() with .traefik.yml testData and no Redis.
func TestNewWithTestDataWithoutRedis(t *testing.T) {
	cfg := CreateConfig()
	cfg.Redis.DSN = "redis://localhost:6379/0"
	cfg.Redis.PoolSize = 10
	cfg.Redis.DialTimeout = "50ms"
	cfg.Redis.Timeout = "1s"
	cfg.Redis.MaxFailures = 5
	cfg.Redis.RetryAfter = "1s"
	cfg.Redis.TLS = RedisTLSConfig{CA: "", InsecureSkipVerify: false}
	cfg.StatusCodes = []string{"200-399"}
	cfg.DefaultTTL = "5m"
	cfg.StaleTTL = "1h"
	cfg.DefaultStaleWhileRevalidate = "0s"
	cfg.DefaultStaleIfError = "0s"
	cfg.MaxBodyBytes = 5242880
	cfg.MaxBufferedBytes = 67108864
	cfg.MaxVariants = 16
	cfg.MaxRevalidations = 10
	cfg.RevalidationTimeout = "30s"
	cfg.SortQuery = false
	cfg.AuthorizationHeaders = []string{}
	cfg.IgnoreClientCacheControl = true
	cfg.ExposeKey = false

	if _, err := New(context.Background(), http.NotFoundHandler(), cfg, "cache"); err != nil {
		t.Fatalf("New() = %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := CreateConfig()
	cfg.DefaultTTL = "nope"
	if _, err := New(context.Background(), http.NotFoundHandler(), cfg, "cache"); err == nil {
		t.Fatal("expected an error")
	}
}
