package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	pastebox "pastebox/internal"
)

func TestStripANSIEscapeSequences(t *testing.T) {
	input := []byte("\x1b[31merror\x1b[0m: interface is up")
	got := stripANSIEscapeSequences(input)
	want := "error: interface is up"

	if string(got) != want {
		t.Fatalf("unexpected ANSI-stripped output: got %q want %q", got, want)
	}
}

func TestLooksLikeTextAcceptsUTF8LogOutput(t *testing.T) {
	input := []byte("en0: 상태=active, 주소=fe80::1\n")
	if !looksLikeText(input) {
		t.Fatalf("expected UTF-8 log output to be treated as text")
	}
}

func TestUploadHandlerKeepsPlainTextResponse(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello world\n"))
	req.Host = "example.com"
	rec := httptest.NewRecorder()

	app.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected text response, got %q", got)
	}
	if !strings.Contains(rec.Body.String(), "주소: http://example.com/") {
		t.Fatalf("expected plain text upload response, got %q", rec.Body.String())
	}
}

func TestUploadHandlerReturnsJSONForJSONRoute(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/week/json", strings.NewReader("hello json\n"))
	req.Host = "example.com"
	rec := httptest.NewRecorder()

	app.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("expected json response, got %q", got)
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}

	if resp.ID == "" {
		t.Fatal("expected non-empty id")
	}
	if resp.URL != "http://example.com/"+resp.ID {
		t.Fatalf("unexpected url %q", resp.URL)
	}
	if !strings.Contains(resp.DeleteURL, resp.URL+"?delete=") {
		t.Fatalf("unexpected delete url %q", resp.DeleteURL)
	}
	if resp.DeleteToken == "" {
		t.Fatal("expected delete token")
	}
	if resp.ExpiresAt == "" {
		t.Fatal("expected expires_at for week policy")
	}
}

func TestUploadHandlerReturnsPasswordForProtectedJSONRoute(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/pw/json", strings.NewReader("secret\n"))
	req.Host = "example.com"
	rec := httptest.NewRecorder()

	app.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}
	if resp.Password == "" {
		t.Fatal("expected generated password")
	}
}

func newTestApp(t *testing.T) *app {
	t.Helper()

	tempDir, err := os.MkdirTemp("", "pastebox-handler-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tempDir)
	})

	store, err := pastebox.NewLocalStore(tempDir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	return &app{
		store:         store,
		maxUploadSize: 1 << 20,
	}
}
