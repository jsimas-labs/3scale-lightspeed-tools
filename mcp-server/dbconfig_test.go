package main

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		leak     string // substring that must NOT appear in the redacted output
	}{
		{"redis://:s3cr3t@redis.example.com:6379/1", "redis.example.com:6379", "s3cr3t"},
		{"rediss://acluser:p4ss@redis.example.com:6380/0", "redis.example.com:6380", "p4ss"},
		{"redis://redis.example.com/1", "redis.example.com:6379", ""},
		{"mysql2://app:dbpw@mysql.example.com/system", "mysql.example.com:3306", "dbpw"},
		{"postgresql://zync:zpw@pg.example.com:5433/zync_production", "pg.example.com:5433", "zpw"},
		{"not a url", "", ""},
	}
	for _, c := range cases {
		red, host := redactURL(c.in)
		if host != c.wantHost {
			t.Errorf("redactURL(%q) host = %q, want %q", c.in, host, c.wantHost)
		}
		if c.leak != "" && strings.Contains(red, c.leak) {
			t.Errorf("redactURL(%q) leaked credential in %q", c.in, red)
		}
	}
}

func TestHostPortOf(t *testing.T) {
	cases := []struct{ in, def, want string }{
		{"sentinel-0.example.com:26379", "26379", "sentinel-0.example.com:26379"},
		{"sentinel-1.example.com", "26379", "sentinel-1.example.com:26379"},
		{"redis://sentinel-2.example.com:26379", "26379", "sentinel-2.example.com:26379"},
		{"", "26379", ""},
	}
	for _, c := range cases {
		if got := hostPortOf(c.in, c.def); got != c.want {
			t.Errorf("hostPortOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
