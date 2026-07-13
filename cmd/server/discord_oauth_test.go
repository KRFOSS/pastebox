package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pastebox "pastebox/internal"
)

func TestDiscordLoginRejectsWhenNoAccountIsLinked(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:     "123456789012345678",
		ClientSecret: "secret",
		RedirectURI:  "https://paste.example.com/ra/discord/callback",
	})
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/login", nil)
	rec := httptest.NewRecorder()

	a.discordLoginHandler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected unlinked login to be rejected with 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "연동하지 않아") {
		t.Fatalf("expected an unlinked account error, got %q", rec.Body.String())
	}
}

func TestDiscordAdminTemplatesParse(t *testing.T) {
	_, _, _, login, dashboard, discord := loadTemplates()
	if login == nil || dashboard == nil || discord == nil {
		t.Fatal("expected all admin templates to load")
	}
}

func TestDiscordLoginCallbackAllowsOnlyLinkedUser(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:       "123456789012345678",
		ClientSecret:   "secret",
		RedirectURI:    "https://paste.example.com/ra/discord/callback",
		LinkedUserID:   "222222222222222222",
		LinkedUsername: "linked-admin",
	})
	restore := useDiscordOAuthTestServer(t, `{"id":"222222222222222222","username":"linked-admin","global_name":"Linked Admin","avatar":"avatar-hash"}`)
	defer restore()

	state := "validOAuthState1234567890"
	storeDiscordOAuthState(t, a, state, discordOAuthIntentLogin)
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected successful login redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/ra" {
		t.Fatalf("expected redirect to /ra, got %q", location)
	}
	if !responseHasCookie(rec.Result(), "admin_token", "master-token") {
		t.Fatal("expected an authenticated admin session cookie")
	}
}

func TestDiscordOAuthSettingsAndStateAreSharedAcrossServers(t *testing.T) {
	shared := newMemoryDiscordOAuthStore()
	nodeA := newDiscordOAuthTestAppWithStore(t, discordOAuthConfig{
		ClientID:       "123456789012345678",
		ClientSecret:   "secret",
		RedirectURI:    "https://paste.example.com/ra/discord/callback",
		LinkedUserID:   "222222222222222222",
		LinkedUsername: "linked-admin",
	}, shared)
	nodeB := newDiscordOAuthTestAppWithStore(t, discordOAuthConfig{}, shared)
	restore := useDiscordOAuthTestServer(t, `{"id":"222222222222222222","username":"linked-admin","global_name":"Linked Admin","avatar":"avatar-hash"}`)
	defer restore()

	startReq := httptest.NewRequest(http.MethodGet, "https://paste.example.com/ra/discord/login", nil)
	startRec := httptest.NewRecorder()
	nodeA.discordLoginHandler(startRec, startReq)
	if startRec.Code != http.StatusFound {
		t.Fatalf("expected node A to start OAuth, got %d: %s", startRec.Code, startRec.Body.String())
	}
	authorizeURL, err := url.Parse(startRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("failed to parse OAuth redirect: %v", err)
	}
	state := authorizeURL.Query().Get("state")
	var stateCookie *http.Cookie
	for _, cookie := range startRec.Result().Cookies() {
		if cookie.Name == discordOAuthStateCookieName {
			stateCookie = cookie
			break
		}
	}
	if state == "" || stateCookie == nil {
		t.Fatal("expected node A to persist an OAuth state and set its cookie")
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	callbackReq.AddCookie(stateCookie)
	callbackRec := httptest.NewRecorder()
	nodeB.discordCallbackHandler(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusSeeOther || callbackRec.Header().Get("Location") != "/ra" {
		t.Fatalf("expected node B to finish OAuth using shared DB state, got %d %q: %s", callbackRec.Code, callbackRec.Header().Get("Location"), callbackRec.Body.String())
	}
	if !responseHasCookie(callbackRec.Result(), "admin_token", "master-token") {
		t.Fatal("expected node B to authenticate the shared linked Discord account")
	}
}

func TestDiscordLoginCallbackRejectsDifferentUser(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:       "123456789012345678",
		ClientSecret:   "secret",
		RedirectURI:    "https://paste.example.com/ra/discord/callback",
		LinkedUserID:   "222222222222222222",
		LinkedUsername: "linked-admin",
	})
	restore := useDiscordOAuthTestServer(t, `{"id":"333333333333333333","username":"not-linked","global_name":"Not Linked","avatar":null}`)
	defer restore()

	state := "validOAuthState1234567890"
	storeDiscordOAuthState(t, a, state, discordOAuthIntentLogin)
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected a different Discord user to be rejected with 403, got %d", rec.Code)
	}
	if responseHasCookie(rec.Result(), "admin_token", "master-token") {
		t.Fatal("a non-linked Discord user must not receive an admin session")
	}
}

func TestDiscordLinkCallbackPersistsSelectedAccount(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:     "123456789012345678",
		ClientSecret: "secret",
		RedirectURI:  "https://paste.example.com/ra/discord/callback",
	})
	restore := useDiscordOAuthTestServer(t, `{"id":"222222222222222222","username":"linked-admin","global_name":"Linked Admin","avatar":"avatar-hash"}`)
	defer restore()

	state := "validOAuthState1234567890"
	storeDiscordOAuthState(t, a, state, discordOAuthIntentLink)
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: "master-token", Path: "/ra"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ra/discord?status=linked" {
		t.Fatalf("expected successful link redirect, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		t.Fatalf("failed to load persisted OAuth settings: %v", err)
	}
	if got := cfg.LinkedUserID; got != "222222222222222222" {
		t.Fatalf("expected linked user ID to update, got %q", got)
	}
}

func TestDiscordCallbackRejectsMismatchedStateCookie(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{})
	state := "validOAuthState1234567890"
	storeDiscordOAuthState(t, a, state, discordOAuthIntentLogin)
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: "different-state", Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected a mismatched state cookie to be rejected, got %d", rec.Code)
	}
	if !a.discordOAuthStore.(*memoryDiscordOAuthStore).hasState(state) {
		t.Fatal("a mismatched state cookie must not consume the server-side OAuth state")
	}
}

func TestDiscordLinkCallbackRequiresActiveAdminSession(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:     "123456789012345678",
		ClientSecret: "secret",
		RedirectURI:  "https://paste.example.com/ra/discord/callback",
	})
	state := "validOAuthState1234567890"
	storeDiscordOAuthState(t, a, state, discordOAuthIntentLink)
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected link callback without an admin session to be rejected with 401, got %d", rec.Code)
	}
	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		t.Fatalf("failed to load OAuth settings: %v", err)
	}
	if cfg.linked() {
		t.Fatal("link callback without an admin session must not update the linked account")
	}
}

func TestDiscordLoginStartUsesIdentifyScopeAndStateCookie(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:       "123456789012345678",
		ClientSecret:   "secret",
		RedirectURI:    "https://paste.example.com/ra/discord/callback",
		LinkedUserID:   "222222222222222222",
		LinkedUsername: "linked-admin",
	})
	req := httptest.NewRequest(http.MethodGet, "https://paste.example.com/ra/discord/login", nil)
	rec := httptest.NewRecorder()

	a.discordLoginHandler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected OAuth redirect, got %d", rec.Code)
	}
	authorizeURL, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("failed to parse OAuth redirect: %v", err)
	}
	if authorizeURL.Query().Get("scope") != "identify" || authorizeURL.Query().Get("response_type") != "code" {
		t.Fatalf("unexpected OAuth parameters: %s", authorizeURL.RawQuery)
	}
	state := authorizeURL.Query().Get("state")
	if state == "" {
		t.Fatal("expected a state parameter")
	}
	var stateCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == discordOAuthStateCookieName {
			stateCookie = cookie
			break
		}
	}
	if stateCookie == nil || stateCookie.Value != state || !stateCookie.HttpOnly || !stateCookie.Secure || stateCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected OAuth state cookie: %#v", stateCookie)
	}
}

func TestDiscordLoginStartAllowsHTTPDevelopmentStateCookie(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:       "123456789012345678",
		ClientSecret:   "secret",
		RedirectURI:    "http://localhost:8080/ra/discord/callback",
		LinkedUserID:   "222222222222222222",
		LinkedUsername: "linked-admin",
	})
	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/ra/discord/login", nil)
	rec := httptest.NewRecorder()

	a.discordLoginHandler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected OAuth redirect, got %d", rec.Code)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == discordOAuthStateCookieName {
			if cookie.Secure {
				t.Fatal("HTTP localhost OAuth state cookie must not use the Secure flag")
			}
			return
		}
	}
	t.Fatal("expected an OAuth state cookie")
}

func TestPersistAdminTokenUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.conf")
	cfg := &config{
		StorageMode: "file",
		ListenAddr:  ":8080",
		DataDir:     "data",
		ExpireDays:  30,
	}

	if err := persistAdminToken(path, cfg, "master-token"); err != nil {
		t.Fatalf("persistAdminToken failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected config permissions 0600, got %o", perm)
	}
}

func TestPersistConfigValuesTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(path, []byte("ADMIN_TOKEN=master-token\n"), 0644); err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("failed to set initial config permissions: %v", err)
	}

	if err := persistConfigValues(path, map[string]string{"MAX_UPLOAD_SIZE_MB": "20"}); err != nil {
		t.Fatalf("persistConfigValues failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected config permissions 0600, got %o", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	if !strings.Contains(string(data), "MAX_UPLOAD_SIZE_MB=20") {
		t.Fatalf("expected updated config value, got %q", data)
	}
}

func TestAdminDiscordSettingsClientIDChangeRequiresRelink(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{
		ClientID:         "123456789012345678",
		ClientSecret:     "old-secret",
		RedirectURI:      "https://paste.example.com/ra/discord/callback",
		LinkedUserID:     "222222222222222222",
		LinkedUsername:   "linked-admin",
		LinkedGlobalName: "Linked Admin",
	})
	form := url.Values{
		"csrf_token":    {"csrf-token"},
		"client_id":     {"987654321098765432"},
		"client_secret": {"new-secret"},
		"redirect_uri":  {"https://paste.example.com/ra/discord/callback"},
	}
	req := httptest.NewRequest(http.MethodPost, "/ra/discord/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: "master-token", Path: "/ra"})
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "csrf-token", Path: "/ra"})
	rec := httptest.NewRecorder()

	a.adminDiscordSettingsHandler(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ra/discord?status=saved-unlinked" {
		t.Fatalf("expected settings update to require relinking, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		t.Fatalf("failed to load OAuth settings: %v", err)
	}
	if cfg.ClientID != "987654321098765432" || cfg.ClientSecret != "new-secret" {
		t.Fatalf("OAuth settings were not updated: %#v", cfg)
	}
	if cfg.linked() {
		t.Fatalf("changing the Client ID must clear the linked account: %#v", cfg)
	}
}

func TestValidateDiscordRedirectURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		ok   bool
	}{
		{"https callback", "https://paste.example.com/ra/discord/callback", true},
		{"localhost callback", "http://localhost:8080/ra/discord/callback", true},
		{"loopback callback", "http://127.0.0.1:8080/ra/discord/callback", true},
		{"insecure public callback", "http://paste.example.com/ra/discord/callback", false},
		{"wrong path", "https://paste.example.com/oauth/callback", false},
		{"query rejected", "https://paste.example.com/ra/discord/callback?next=/ra", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDiscordRedirectURI(test.uri)
			if (err == nil) != test.ok {
				t.Fatalf("validateDiscordRedirectURI(%q) error = %v, want ok=%v", test.uri, err, test.ok)
			}
		})
	}
}

func newDiscordOAuthTestApp(t *testing.T, cfg discordOAuthConfig) *app {
	t.Helper()
	return newDiscordOAuthTestAppWithStore(t, cfg, newMemoryDiscordOAuthStore())
}

func newDiscordOAuthTestAppWithStore(t *testing.T, cfg discordOAuthConfig, store *memoryDiscordOAuthStore) *app {
	t.Helper()
	if cfg != (discordOAuthConfig{}) {
		if err := store.SaveDiscordOAuthSettings(cfg.storageSettings()); err != nil {
			t.Fatalf("failed to seed OAuth settings: %v", err)
		}
	}
	return &app{
		adminLogin:        template.Must(template.New("login").Parse(`{{ .Error }}`)),
		adminToken:        "master-token",
		configPath:        filepath.Join(t.TempDir(), "config.conf"),
		discordOAuthStore: store,
		discordHTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
}

func storeDiscordOAuthState(t *testing.T, a *app, state, intent string) {
	t.Helper()
	if err := a.discordOAuthStore.CreateDiscordOAuthState(pastebox.DiscordOAuthPendingState{
		State:     state,
		Intent:    intent,
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("failed to store OAuth state: %v", err)
	}
}

type memoryDiscordOAuthStore struct {
	mu          sync.Mutex
	settings    pastebox.DiscordOAuthSettings
	hasSettings bool
	states      map[string]pastebox.DiscordOAuthPendingState
}

func newMemoryDiscordOAuthStore() *memoryDiscordOAuthStore {
	return &memoryDiscordOAuthStore{states: make(map[string]pastebox.DiscordOAuthPendingState)}
}

func (s *memoryDiscordOAuthStore) LoadDiscordOAuthSettings() (pastebox.DiscordOAuthSettings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings, s.hasSettings, nil
}

func (s *memoryDiscordOAuthStore) SaveDiscordOAuthSettings(settings pastebox.DiscordOAuthSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = settings
	s.hasSettings = true
	return nil
}

func (s *memoryDiscordOAuthStore) CreateDiscordOAuthState(state pastebox.DiscordOAuthPendingState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.states[state.State]; exists {
		return os.ErrExist
	}
	s.states[state.State] = state
	return nil
}

func (s *memoryDiscordOAuthStore) ConsumeDiscordOAuthState(state string, now time.Time) (pastebox.DiscordOAuthPendingState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, exists := s.states[state]
	if !exists {
		return pastebox.DiscordOAuthPendingState{}, false, nil
	}
	delete(s.states, state)
	if !pending.ExpiresAt.After(now) {
		return pastebox.DiscordOAuthPendingState{}, false, nil
	}
	return pending, true, nil
}

func (s *memoryDiscordOAuthStore) hasState(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.states[state]
	return exists
}

func useDiscordOAuthTestServer(t *testing.T, userJSON string) func() {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			clientID, clientSecret, ok := r.BasicAuth()
			if !ok || clientID != "123456789012345678" || clientSecret != "secret" {
				http.Error(w, "bad client credentials", http.StatusUnauthorized)
				return
			}
			if err := r.ParseForm(); err != nil || r.FormValue("grant_type") != "authorization_code" || r.FormValue("code") != "test-code" {
				http.Error(w, "bad token request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"oauth-access-token","token_type":"Bearer"}`))
		case "/users/@me":
			if r.Header.Get("Authorization") != "Bearer oauth-access-token" {
				http.Error(w, "bad bearer token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(userJSON))
		default:
			http.NotFound(w, r)
		}
	}))

	oldTokenURL := discordOAuthTokenURL
	oldUserURL := discordOAuthUserURL
	discordOAuthTokenURL = server.URL + "/token"
	discordOAuthUserURL = server.URL + "/users/@me"
	return func() {
		discordOAuthTokenURL = oldTokenURL
		discordOAuthUserURL = oldUserURL
		server.Close()
	}
}

func responseHasCookie(response *http.Response, name, value string) bool {
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.Value == value {
			return true
		}
	}
	return false
}
