package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	a.discordOAuthStates[state] = discordOAuthState{Intent: discordOAuthIntentLogin, ExpiresAt: time.Now().Add(time.Minute)}
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
	a.discordOAuthStates[state] = discordOAuthState{Intent: discordOAuthIntentLogin, ExpiresAt: time.Now().Add(time.Minute)}
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
	a.discordOAuthStates[state] = discordOAuthState{Intent: discordOAuthIntentLink, ExpiresAt: time.Now().Add(time.Minute)}
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: "master-token", Path: "/ra"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ra/discord?status=linked" {
		t.Fatalf("expected successful link redirect, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := a.getDiscordOAuthConfig().LinkedUserID; got != "222222222222222222" {
		t.Fatalf("expected linked user ID to update, got %q", got)
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatalf("failed to read persisted config: %v", err)
	}
	if !strings.Contains(string(data), "DISCORD_OAUTH_LINKED_USER_ID=222222222222222222") {
		t.Fatalf("linked user was not persisted: %s", data)
	}
	info, err := os.Stat(a.configPath)
	if err != nil {
		t.Fatalf("failed to stat persisted config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected config permissions 0600, got %o", perm)
	}
}

func TestDiscordCallbackRejectsMismatchedStateCookie(t *testing.T) {
	a := newDiscordOAuthTestApp(t, discordOAuthConfig{})
	state := "validOAuthState1234567890"
	a.discordOAuthStates[state] = discordOAuthState{Intent: discordOAuthIntentLogin, ExpiresAt: time.Now().Add(time.Minute)}
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: "different-state", Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected a mismatched state cookie to be rejected, got %d", rec.Code)
	}
	if _, ok := a.discordOAuthStates[state]; !ok {
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
	a.discordOAuthStates[state] = discordOAuthState{Intent: discordOAuthIntentLink, ExpiresAt: time.Now().Add(time.Minute)}
	req := httptest.NewRequest(http.MethodGet, "/ra/discord/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: discordOAuthStateCookieName, Value: state, Path: "/ra/discord"})
	rec := httptest.NewRecorder()

	a.discordCallbackHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected link callback without an admin session to be rejected with 401, got %d", rec.Code)
	}
	if a.getDiscordOAuthConfig().linked() {
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
	cfg := a.getDiscordOAuthConfig()
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
	return &app{
		adminLogin:         template.Must(template.New("login").Parse(`{{ .Error }}`)),
		adminToken:         "master-token",
		configPath:         filepath.Join(t.TempDir(), "config.conf"),
		discordOAuth:       cfg,
		discordOAuthStates: make(map[string]discordOAuthState),
		discordHTTPClient:  &http.Client{Timeout: 2 * time.Second},
	}
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
