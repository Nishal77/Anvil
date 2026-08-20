package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHandlePutSecret_StoresUnderAuthenticatedUser(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	var gotUserID uuid.UUID
	var gotName, gotValue string
	a := &fakeAuth{
		verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil },
		putSecretFn: func(_ context.Context, id uuid.UUID, name, plaintext string) error {
			gotUserID, gotName, gotValue = id, name, plaintext
			return nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/secrets", strings.NewReader(`{"name":"GITHUB_TOKEN","value":"ghp_test"}`))
	req.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusNoContent, w.Body)
	}
	if gotUserID != userID {
		t.Errorf("PutSecret called with user %s, want %s", gotUserID, userID)
	}
	if gotName != "GITHUB_TOKEN" || gotValue != "ghp_test" {
		t.Errorf("PutSecret called with (%q, %q), want (%q, %q)", gotName, gotValue, "GITHUB_TOKEN", "ghp_test")
	}
}

func TestHandlePutSecret_RejectsEmptyValue(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	a := &fakeAuth{verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil }}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/secrets", strings.NewReader(`{"name":"GITHUB_TOKEN","value":""}`))
	req.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleListSecretNames_ReturnsNamesOnly proves the response body
// carries only names, never a value field — PRD §16.5's "no read-back
// endpoint, ever" would be trivially violated by a response that
// echoed the stored plaintext or ciphertext.
func TestHandleListSecretNames_ReturnsNamesOnly(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	a := &fakeAuth{
		verifyFn:          func(_ string) (uuid.UUID, error) { return userID, nil },
		listSecretNamesFn: func(_ context.Context, _ uuid.UUID) ([]string, error) { return []string{"GITHUB_TOKEN"}, nil },
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/secrets", nil)
	req.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(w.Body.String(), "ciphertext") || strings.Contains(w.Body.String(), "value") {
		t.Fatalf("response leaked more than names: %s", w.Body)
	}
	var got secretNamesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Names) != 1 || got.Names[0] != "GITHUB_TOKEN" {
		t.Errorf("Names = %v, want [GITHUB_TOKEN]", got.Names)
	}
}

func TestHandleDeleteSecret_DeletesByName(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	var gotName string
	a := &fakeAuth{
		verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil },
		deleteSecretFn: func(_ context.Context, _ uuid.UUID, name string) error {
			gotName = name
			return nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodDelete, "/secrets/GITHUB_TOKEN", nil)
	req.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusNoContent, w.Body)
	}
	if gotName != "GITHUB_TOKEN" {
		t.Errorf("DeleteSecret called with name %q, want %q", gotName, "GITHUB_TOKEN")
	}
}

func TestHandlePutSecret_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/secrets", strings.NewReader(`{"name":"X","value":"Y"}`))
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}
