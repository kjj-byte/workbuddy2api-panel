package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlatformAdminEndpointsRequireKey(t *testing.T) {
	p := newTestPanel()
	for _, route := range []struct{ method, path string }{
		{"GET", "/panel/api/platforms"}, {"POST", "/panel/api/platforms/rpc"}, {"POST", "/panel/api/platforms/schedule"}, {"POST", "/panel/api/platforms/checkin"},
	} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(route.method, route.path, strings.NewReader(`{"method":"backup.export","payload":{}}`)))
		if w.Code != 401 {
			t.Fatalf("%s: expected auth failure, got %d", route.path, w.Code)
		}
	}
}
func TestPlatformAdminRejectsArbitraryWorkerOperations(t *testing.T) {
	p := newTestPanel()
	r := httptest.NewRequest("POST", "/panel/api/platforms/rpc", strings.NewReader(`{"method":"close","payload":{}}`))
	r.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("worker control must be rejected, got %d", w.Code)
	}
}

func TestPlatformUnsupportedClaimIsBadRequest(t *testing.T) {
	p := newTestPanel()
	r := httptest.NewRequest("POST", "/panel/api/platforms/checkin", strings.NewReader(`{"provider":"cline"}`))
	r.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("unsupported checkin: %d", w.Code)
	}
}
