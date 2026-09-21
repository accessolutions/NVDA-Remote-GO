package server

import (
	"testing"
	"time"
)

func TestValidateHTTPRelayConfiguration(t *testing.T) {
	config := defaultHTTPRelayConfig()
	tests := []struct {
		name      string
		enabled   bool
		path      string
		v2Enabled bool
		v2Path    string
		adminPath string
		addresses int
		config    httpRelayConfig
		wantError bool
	}{
		{name: "valid", enabled: true, path: "/remote/http/v1", v2Enabled: true, v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 1, config: config},
		{name: "disabled without listener", enabled: false, path: "/remote/http/v1", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: config},
		{name: "enabled without listener", enabled: true, path: "/remote/http/v1", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: config, wantError: true},
		{name: "v2 without v1", enabled: false, path: "/remote/http/v1", v2Enabled: true, v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 1, config: config, wantError: true},
		{name: "missing leading slash", enabled: false, path: "remote/http/v1", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: config, wantError: true},
		{name: "admin path conflict", enabled: false, path: "/admin/", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: config, wantError: true},
		{name: "v2 path conflict", enabled: false, path: "/remote/http/v1", v2Path: "/admin/", adminPath: "/admin", addresses: 0, config: config, wantError: true},
		{name: "poll wait too long", enabled: false, path: "/remote/http/v1", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: httpRelayConfig{pollWait: 2 * time.Second, sessionTTL: 2 * time.Second, maxSessions: 1, maxSessionsPerIP: 1, maxMessageBytes: 1, maxBatchMessages: 1, maxPendingBytes: 1, streamKeepalive: time.Second}, wantError: true},
		{name: "invalid limit", enabled: false, path: "/remote/http/v1", v2Path: "/remote/http/v2", adminPath: "/admin", addresses: 0, config: httpRelayConfig{pollWait: time.Second, sessionTTL: 2 * time.Second, maxSessions: 0, maxSessionsPerIP: 1, maxMessageBytes: 1, maxBatchMessages: 1, maxPendingBytes: 1, streamKeepalive: time.Second}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateHTTPRelayConfiguration(test.enabled, test.path, test.v2Enabled, test.v2Path, test.adminPath, test.addresses, test.config)
			if (err != nil) != test.wantError {
				t.Fatalf("validation error = %v, wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestNormalizeHTTPRelayPath(t *testing.T) {
	for input, expected := range map[string]string{
		"":                 "/remote/http/v1",
		"remote/http/v1/":  "/remote/http/v1",
		"/remote/http/v1/": "/remote/http/v1",
		"/":                "/",
	} {
		if got := normalizeHTTPRelayPath(input); got != expected {
			t.Errorf("normalizeHTTPRelayPath(%q) = %q, want %q", input, got, expected)
		}
	}
}
