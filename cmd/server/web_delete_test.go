package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pastebox "pastebox/internal"
)

func TestForceDeleteQueryRequiresWebDeletePost(t *testing.T) {
	app := newTestApp(t)
	id, _ := createWebDeleteTestPaste(t, app)

	req := httptest.NewRequest(http.MethodGet, "/"+id+"?delete=force", nil)
	rec := httptest.NewRecorder()

	webDeleteProtection(http.HandlerFunc(app.handle)).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", rec.Code)
	}
	if _, err := app.store.Stat(id, ""); err != nil {
		t.Fatalf("paste must remain after rejected force delete: %v", err)
	}
}

func TestWebDeletePostRequiresNonce(t *testing.T) {
	app := newTestApp(t)
	id, _ := createWebDeleteTestPaste(t, app)

	req := httptest.NewRequest(http.MethodPost, "/"+id+"?delete=force", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	webDeleteProtection(http.HandlerFunc(app.handle)).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", rec.Code)
	}
	if _, err := app.store.Stat(id, ""); err != nil {
		t.Fatalf("paste must remain after missing nonce: %v", err)
	}
}

func TestWebDeletePostWithNonceAllowsForceDelete(t *testing.T) {
	app := newTestApp(t)
	id, _ := createWebDeleteTestPaste(t, app)
	nonce := "nonce-for-test"

	req := httptest.NewRequest(http.MethodPost, "/"+id+"?delete=force", strings.NewReader(webDeleteNonceField+"="+nonce))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{
		Name:  webDeleteNonceCookieName(id),
		Value: nonce,
		Path:  "/" + id,
	})
	rec := httptest.NewRecorder()

	webDeleteProtection(http.HandlerFunc(app.handle)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if _, err := app.store.Stat(id, ""); !errors.Is(err, pastebox.ErrNotFound) {
		t.Fatalf("expected paste to be deleted, got %v", err)
	}
}

func TestDeleteTokenStillWorksForCommandLineDelete(t *testing.T) {
	app := newTestApp(t)
	id, deleteToken := createWebDeleteTestPaste(t, app)

	req := httptest.NewRequest(http.MethodGet, "/"+id+"?delete="+deleteToken, nil)
	rec := httptest.NewRecorder()

	webDeleteProtection(http.HandlerFunc(app.handle)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if _, err := app.store.Stat(id, ""); !errors.Is(err, pastebox.ErrNotFound) {
		t.Fatalf("expected paste to be deleted, got %v", err)
	}
}

func createWebDeleteTestPaste(t *testing.T, app *app) (string, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader("hello\n"))
	req.Host = "example.com"
	rec := httptest.NewRecorder()

	app.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode upload response: %v", err)
	}
	if resp.ID == "" || resp.DeleteToken == "" {
		t.Fatalf("expected id and delete token, got %#v", resp)
	}

	return resp.ID, resp.DeleteToken
}
