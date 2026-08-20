package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCaddyClient_RegisterRoute_PostsToCorrectPath(t *testing.T) {
	t.Parallel()
	var gotPath, gotMethod string
	var gotRoute caddyRoute
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotRoute)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewCaddyClient(CaddyConfig{AdminAddr: server.URL})
	if err != nil {
		t.Fatalf("NewCaddyClient() error = %v", err)
	}

	if err := c.RegisterRoute(context.Background(), "abc123.preview.anvil.dev", 54321); err != nil {
		t.Fatalf("RegisterRoute() error = %v", err)
	}

	wantPath := "/config/apps/http/servers/" + caddyServerName + "/routes"
	if gotPath != wantPath || gotMethod != http.MethodPost {
		t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, wantPath)
	}
	assertRegisteredRoute(t, gotRoute)
}

// assertRegisteredRoute checks the route shape RegisterRoute
// constructs — split out of the test function that calls it purely to
// keep that function's branching within CLAUDE.md's
// cyclomatic-complexity limit.
func assertRegisteredRoute(t *testing.T, gotRoute caddyRoute) {
	t.Helper()
	if gotRoute.ID != "preview-abc123.preview.anvil.dev" {
		t.Errorf("route @id = %q, want a stable id derived from the hostname", gotRoute.ID)
	}
	if len(gotRoute.Match) != 1 || len(gotRoute.Match[0].Host) != 1 || gotRoute.Match[0].Host[0] != "abc123.preview.anvil.dev" {
		t.Errorf("route match = %+v, want host match on the given hostname", gotRoute.Match)
	}
	if len(gotRoute.Handle) != 1 || len(gotRoute.Handle[0].Upstreams) != 1 || gotRoute.Handle[0].Upstreams[0].Dial != "127.0.0.1:54321" {
		t.Errorf("route handle = %+v, want a reverse_proxy upstream at 127.0.0.1:54321", gotRoute.Handle)
	}
}

func TestCaddyClient_RegisterRoute_NonOKStatusIsError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c, err := NewCaddyClient(CaddyConfig{AdminAddr: server.URL})
	if err != nil {
		t.Fatalf("NewCaddyClient() error = %v", err)
	}
	if err := c.RegisterRoute(context.Background(), "h", 1); err == nil {
		t.Fatal("RegisterRoute() error = nil, want an error on a non-200 response")
	}
}

func TestCaddyClient_RemoveRoute_DeletesByID(t *testing.T) {
	t.Parallel()
	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewCaddyClient(CaddyConfig{AdminAddr: server.URL})
	if err != nil {
		t.Fatalf("NewCaddyClient() error = %v", err)
	}
	if err := c.RemoveRoute(context.Background(), "abc123.preview.anvil.dev"); err != nil {
		t.Fatalf("RemoveRoute() error = %v", err)
	}

	wantPath := "/id/preview-abc123.preview.anvil.dev"
	if gotPath != wantPath || gotMethod != http.MethodDelete {
		t.Errorf("request = %s %s, want DELETE %s", gotMethod, gotPath, wantPath)
	}
}

// TestCaddyClient_RemoveRoute_NotFoundIsNotAnError proves removing an
// already-gone route is idempotent, matching every other Destroy-style
// operation in this codebase.
func TestCaddyClient_RemoveRoute_NotFoundIsNotAnError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c, err := NewCaddyClient(CaddyConfig{AdminAddr: server.URL})
	if err != nil {
		t.Fatalf("NewCaddyClient() error = %v", err)
	}
	if err := c.RemoveRoute(context.Background(), "h"); err != nil {
		t.Errorf("RemoveRoute() error = %v, want nil for an already-gone route", err)
	}
}

func TestNewCaddyClient_RejectsEmptyAdminAddr(t *testing.T) {
	t.Parallel()
	if _, err := NewCaddyClient(CaddyConfig{}); err == nil {
		t.Fatal("NewCaddyClient() error = nil, want an error for an empty AdminAddr")
	}
}
