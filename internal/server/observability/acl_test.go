// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func parseCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var result []*net.IPNet
	for _, s := range cidrs {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("invalid test CIDR %q: %v", s, err)
		}
		result = append(result, cidr)
	}
	return result
}

func TestACL_Allowed(t *testing.T) {
	cases := []struct {
		name    string
		cidrs   []string
		remote  string
		allowed bool
	}{
		{"localhost allowed", []string{"127.0.0.1/32"}, "127.0.0.1:54321", true},
		{"localhost denied by other CIDR", []string{"10.0.0.0/8"}, "127.0.0.1:54321", false},
		{"10.0.0.5 in 10.0.0.0/8", []string{"10.0.0.0/8"}, "10.0.0.5:1234", true},
		{"192.168.1.100 in /24", []string{"192.168.1.0/24"}, "192.168.1.100:80", true},
		{"192.168.2.1 NOT in 192.168.1.0/24", []string{"192.168.1.0/24"}, "192.168.2.1:80", false},
		{"multiple CIDRs, second matches", []string{"10.0.0.0/8", "192.168.1.0/24"}, "192.168.1.50:80", true},
		{"empty CIDR list denies everything", nil, "127.0.0.1:80", false},
		{"IP without port", []string{"127.0.0.1/32"}, "127.0.0.1", true},
		{"invalid remote addr", []string{"127.0.0.1/32"}, "not-an-ip", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl := NewACL(parseCIDRs(t, tc.cidrs...))
			if got := acl.Allowed(tc.remote); got != tc.allowed {
				t.Errorf("Allowed(%q) = %v, want %v", tc.remote, got, tc.allowed)
			}
		})
	}
}

func TestACL_Middleware(t *testing.T) {
	acl := NewACL(parseCIDRs(t, "127.0.0.1/32"))

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := acl.Middleware(okHandler)

	t.Run("allowed IP passes through", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("denied IP gets 403", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d", rec.Code)
		}
	})
}
