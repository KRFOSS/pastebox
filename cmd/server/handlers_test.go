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

func TestCurlUploadIsRenderedAsText(t *testing.T) {
	app := newTestApp(t)

	upload := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello from curl\n"))
	upload.Header.Set("User-Agent", "curl/8.0.1")
	upload.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	upload.Host = "example.com"
	uploadRec := httptest.NewRecorder()
	app.handle(uploadRec, upload)

	if uploadRec.Code != http.StatusOK {
		t.Fatalf("expected upload status 200, got %d", uploadRec.Code)
	}
	url := strings.TrimPrefix(strings.SplitN(uploadRec.Body.String(), "\n", 2)[0], "주소: ")
	id := strings.TrimPrefix(url, "http://example.com/")

	raw := httptest.NewRequest(http.MethodGet, "/"+id+"?raw=1", nil)
	raw.Header.Set("User-Agent", "Mozilla/5.0")
	raw.Header.Set("Accept", "text/html")
	rawRec := httptest.NewRecorder()
	app.handle(rawRec, raw)

	if got := rawRec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected text raw response, got %q", got)
	}
	if got := rawRec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("expected no download disposition, got %q", got)
	}
	if got := rawRec.Body.String(); got != "hello from curl\n" {
		t.Fatalf("unexpected raw body %q", got)
	}
}

func TestLegacyCurlUploadIsRenderedAsText(t *testing.T) {
	app := newTestApp(t)
	meta, _, _, err := app.store.Create(
		strings.NewReader("legacy curl upload\n"),
		"application/x-www-form-urlencoded",
		false, "", "", 0,
	)
	if err != nil {
		t.Fatalf("failed to create legacy paste: %v", err)
	}

	raw := httptest.NewRequest(http.MethodGet, "/"+meta.ID+"?raw=1", nil)
	raw.Header.Set("User-Agent", "Mozilla/5.0")
	raw.Header.Set("Accept", "text/html")
	rawRec := httptest.NewRecorder()
	app.handle(rawRec, raw)

	if got := rawRec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("expected text raw response, got %q", got)
	}
	if got := rawRec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("expected no download disposition, got %q", got)
	}
	if got := rawRec.Body.String(); got != "legacy curl upload\n" {
		t.Fatalf("unexpected raw body %q", got)
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

func TestParsePwUploadPath(t *testing.T) {
	cases := []struct {
		path   string
		custom string
		policy string
		ok     bool
	}{
		{"/pw/12345", "12345", "", true},
		{"/pw/12345/json", "12345", "", true},
		{"/pw/12345/temp", "12345", "once", true},
		{"/pw/12345/week", "12345", "week", true},
		{"/pw/temp/temp", "temp", "once", true}, // 비번이 temp
		{"/pw/week/week", "week", "week", true}, // 비번이 week
		{"/pw/temp", "", "once", true},          // 단독 정책: 비번은 헤더 필요
		{"/pw/week", "", "week", true},
		{"/pw/week/json", "", "week", true},
		{"/pw", "", "", false},      // 랜덤 (별도 처리)
		{"/pw/json", "", "", false}, // 랜덤+json (별도 처리)
		{"/pw/", "", "", false},
		{"/pw/a/b", "", "", false},       // 두번째 세그먼트는 정책이어야 함
		{"/pw/12345/foo", "", "", false}, // 알 수 없는 정책
		{"/", "", "", false},
		{"/abcde", "", "", false},
	}

	for _, c := range cases {
		custom, policy, ok := parsePwUploadPath(c.path)
		if custom != c.custom || policy != c.policy || ok != c.ok {
			t.Errorf("parsePwUploadPath(%q) = (%q, %q, %v), want (%q, %q, %v)", c.path, custom, policy, ok, c.custom, c.policy, c.ok)
		}
	}
}

func TestUploadHandlerCustomPasswordViaPath(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/pw/12345/json", strings.NewReader("secret data\n"))
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
	if resp.Password != "12345" {
		t.Fatalf("expected custom password %q, got %q", "12345", resp.Password)
	}

	// 잘못된 비밀번호는 거부, 지정한 비밀번호로는 열람 가능해야 함
	if _, err := app.store.Stat(resp.ID, "wrong"); err != pastebox.ErrInvalidPassword {
		t.Fatalf("expected ErrInvalidPassword for wrong password, got %v", err)
	}
	entry, err := app.store.Open(resp.ID, "12345")
	if err != nil {
		t.Fatalf("expected to open with custom password, got %v", err)
	}
	_ = entry.File.Close()
}

func TestUploadHandlerCustomPasswordWithWeekPolicy(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/pw/secret/week/json", strings.NewReader("data\n"))
	req.Host = "example.com"
	rec := httptest.NewRecorder()

	app.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}
	if resp.Password != "secret" {
		t.Fatalf("expected custom password %q, got %q", "secret", resp.Password)
	}
	if resp.ExpiresAt == "" {
		t.Fatal("expected expires_at for week policy")
	}

	meta, err := app.store.Stat(resp.ID, "secret")
	if err != nil {
		t.Fatalf("stat with custom password failed: %v", err)
	}
	if meta.DataPolicy != "week" {
		t.Fatalf("expected week policy, got %q", meta.DataPolicy)
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
