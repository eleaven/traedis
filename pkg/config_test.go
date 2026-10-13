package traedis

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCreateConfigDefaults(t *testing.T) {
	s, err := parseConfig(CreateConfig())
	if err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if s.redis.addr != "cache:6379" || s.redis.db != 0 || s.redis.timeout != 50*time.Millisecond {
		t.Errorf("redis = %+v", s.redis)
	}
	if len(s.statusCodes) != 1 || s.statusCodes[0] != 200 {
		t.Errorf("statusCodes = %v", s.statusCodes)
	}
	if s.defaultTTL != 5*time.Minute || s.staleTTL != time.Hour {
		t.Errorf("defaultTTL = %v, staleTTL = %v", s.defaultTTL, s.staleTTL)
	}
	if s.defaultStaleWhileRevalidate != 0 || s.defaultStaleIfError != 0 {
		t.Errorf("§4.2.4 no stale response unless the backend or the configuration allows it: %v, %v", s.defaultStaleWhileRevalidate, s.defaultStaleIfError)
	}
	if s.maxBodyBytes != 5242880 || s.maxVariants != 16 || s.sortQuery || s.exposeKey {
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
		{name: "invalid status code", mutate: func(c *Config) { c.StatusCodes = []int{200, 999} }, wantErr: "statusCodes"},
		{name: "no status code", mutate: func(c *Config) { c.StatusCodes = []int{} }, wantErr: "statusCodes must not be empty"},
		{name: "informational status code", mutate: func(c *Config) { c.StatusCodes = []int{200, 103} }, wantErr: "a 103 response is never stored"},
		{name: "§3.3 partial content", mutate: func(c *Config) { c.StatusCodes = []int{200, 206} }, wantErr: "a 206 response is never stored"},
		{name: "§3.2 not modified", mutate: func(c *Config) { c.StatusCodes = []int{304} }, wantErr: "a 304 response is never stored"},
		{name: "zero maxBodyBytes", mutate: func(c *Config) { c.MaxBodyBytes = 0 }, wantErr: "maxBodyBytes"},
		{name: "maxBodyBytes past what Redis takes", mutate: func(c *Config) { c.MaxBodyBytes = 512<<20 + 1 }, wantErr: "maxBodyBytes must not exceed"},
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
		{name: "dsn scheme must be redis", mutate: func(c *Config) { c.Redis.DSN = "rediss://cache:6379" }, wantErr: "scheme"},
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
		"rediss://user:s3cret@cache",
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
	cfg.Redis.Timeout = "50ms"
	cfg.StatusCodes = []int{200}
	cfg.DefaultTTL = "5m"
	cfg.StaleTTL = "1h"
	cfg.DefaultStaleWhileRevalidate = "0s"
	cfg.DefaultStaleIfError = "0s"
	cfg.MaxBodyBytes = 5242880
	cfg.MaxVariants = 16
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
