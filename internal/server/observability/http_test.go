// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockMetrics implementa HandlerMetrics para testes.
type mockMetrics struct {
	data MetricsData
}

func (m *mockMetrics) MetricsSnapshot() MetricsData {
	return m.data
}

func TestHealth_ReturnsOK(t *testing.T) {
	acl := NewACL(parseCIDRs(t, "127.0.0.1/32"))
	router := NewRouter(&mockMetrics{}, nil, acl)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", resp["status"])
	}
	if _, ok := resp["uptime"]; !ok {
		t.Error("expected uptime field")
	}
	if _, ok := resp["version"]; !ok {
		t.Error("expected version field")
	}
}

func TestMetrics_ReturnsData(t *testing.T) {
	mock := &mockMetrics{
		data: MetricsData{
			TrafficIn:   1024 * 1024,
			DiskWrite:   512 * 1024,
			ActiveConns: 3,
		},
	}
	acl := NewACL(parseCIDRs(t, "127.0.0.1/32"))
	router := NewRouter(mock, nil, acl)

	req := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if int64(resp["traffic_in_bytes"].(float64)) != 1024*1024 {
		t.Errorf("expected traffic_in_bytes %d, got %v", 1024*1024, resp["traffic_in_bytes"])
	}
	if int32(resp["active_conns"].(float64)) != 3 {
		t.Errorf("expected active_conns 3, got %v", resp["active_conns"])
	}
}

func TestACL_BlocksHealthEndpoint(t *testing.T) {
	// ACL só permite 10.0.0.0/8
	acl := NewACL([]*net.IPNet{
		mustParseCIDR("10.0.0.0/8"),
	})
	router := NewRouter(&mockMetrics{}, nil, acl)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.RemoteAddr = "192.168.1.1:12345" // não permitido
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestRoot_ReturnsSPA(t *testing.T) {
	acl := NewACL(parseCIDRs(t, "127.0.0.1/32"))
	router := NewRouter(&mockMetrics{}, nil, acl)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected Content-Type text/html, got %q", ct)
	}
}

func TestNotFound_Returns404(t *testing.T) {
	acl := NewACL(parseCIDRs(t, "127.0.0.1/32"))
	router := NewRouter(&mockMetrics{}, nil, acl)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func mustParseCIDR(s string) *net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return cidr
}
