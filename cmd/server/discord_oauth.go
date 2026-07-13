package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	pastebox "pastebox/internal"
)

const (
	discordOAuthStateCookieName = "discord_oauth_state"
	discordOAuthStateMaxAge     = 10 * time.Minute
	discordOAuthIntentLink      = "link"
	discordOAuthIntentLogin     = "login"
)

var (
	discordOAuthAuthorizeURL = "https://discord.com/oauth2/authorize"
	discordOAuthTokenURL     = "https://discord.com/api/oauth2/token"
	discordOAuthUserURL      = "https://discord.com/api/v10/users/@me"
	discordSnowflakePattern  = regexp.MustCompile(`^[0-9]{17,20}$`)
	errDiscordOAuthDBOnly    = errors.New("Discord OAuth requires database storage mode")
)

type discordOAuthConfig struct {
	ClientID         string
	ClientSecret     string
	RedirectURI      string
	LinkedUserID     string
	LinkedUsername   string
	LinkedGlobalName string
	LinkedAvatar     string
}

func (c discordOAuthConfig) configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURI != ""
}

func (c discordOAuthConfig) linked() bool {
	return c.LinkedUserID != ""
}

func (c discordOAuthConfig) ready() bool {
	return c.configured() && c.linked()
}

func (c discordOAuthConfig) linkedDisplayName() string {
	if c.LinkedGlobalName != "" {
		return c.LinkedGlobalName
	}
	return c.LinkedUsername
}

func (c discordOAuthConfig) linkedAvatarURL() string {
	if c.LinkedUserID == "" || c.LinkedAvatar == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=128", c.LinkedUserID, c.LinkedAvatar)
}

type discordTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

func discordOAuthConfigFromStorage(settings pastebox.DiscordOAuthSettings) discordOAuthConfig {
	return discordOAuthConfig{
		ClientID:         settings.ClientID,
		ClientSecret:     settings.ClientSecret,
		RedirectURI:      settings.RedirectURI,
		LinkedUserID:     settings.LinkedUserID,
		LinkedUsername:   settings.LinkedUsername,
		LinkedGlobalName: settings.LinkedGlobalName,
		LinkedAvatar:     settings.LinkedAvatar,
	}
}

func (c discordOAuthConfig) storageSettings() pastebox.DiscordOAuthSettings {
	return pastebox.DiscordOAuthSettings{
		ClientID:         c.ClientID,
		ClientSecret:     c.ClientSecret,
		RedirectURI:      c.RedirectURI,
		LinkedUserID:     c.LinkedUserID,
		LinkedUsername:   c.LinkedUsername,
		LinkedGlobalName: c.LinkedGlobalName,
		LinkedAvatar:     c.LinkedAvatar,
	}
}

func (a *app) getDiscordOAuthConfig() (discordOAuthConfig, error) {
	if a.discordOAuthStore == nil {
		return discordOAuthConfig{}, errDiscordOAuthDBOnly
	}
	settings, found, err := a.discordOAuthStore.LoadDiscordOAuthSettings()
	if err != nil {
		return discordOAuthConfig{}, err
	}
	if !found {
		return discordOAuthConfig{}, nil
	}
	return discordOAuthConfigFromStorage(settings), nil
}

func (a *app) discordOAuthReady() bool {
	cfg, err := a.getDiscordOAuthConfig()
	return err == nil && cfg.ready()
}

func (a *app) discordHTTP() *http.Client {
	if a.discordHTTPClient != nil {
		return a.discordHTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (a *app) saveDiscordOAuthConfig(next discordOAuthConfig) error {
	if a.discordOAuthStore == nil {
		return errDiscordOAuthDBOnly
	}
	return a.discordOAuthStore.SaveDiscordOAuthSettings(next.storageSettings())
}

func (a *app) renderAdminLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	_ = a.adminLogin.Execute(w, map[string]any{
		"Error":             message,
		"DiscordOAuthReady": a.discordOAuthReady(),
	})
}

func (a *app) setAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_token",
		Value:    a.adminToken,
		Path:     "/ra",
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
}

func (a *app) adminDiscordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminAuthenticated(r) {
		http.Redirect(w, r, "/ra", http.StatusSeeOther)
		return
	}

	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		http.Error(w, "Discord OAuth 설정을 데이터베이스에서 불러오지 못했습니다.", http.StatusServiceUnavailable)
		return
	}
	notice, noticeError := discordAdminNotice(r.URL.Query().Get("status"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.adminDiscord.Execute(w, map[string]any{
		"CSRFToken":              a.setCSRFCookie(w, r),
		"ClientID":               cfg.ClientID,
		"RedirectURI":            cfg.RedirectURI,
		"ClientSecretConfigured": cfg.ClientSecret != "",
		"OAuthConfigured":        cfg.configured(),
		"DiscordLinked":          cfg.linked(),
		"LinkedUserID":           cfg.LinkedUserID,
		"LinkedUsername":         cfg.LinkedUsername,
		"LinkedDisplayName":      cfg.linkedDisplayName(),
		"LinkedAvatarURL":        cfg.linkedAvatarURL(),
		"Notice":                 notice,
		"NoticeError":            noticeError,
	})
}

func discordAdminNotice(status string) (string, bool) {
	switch status {
	case "saved":
		return "Discord OAuth 설정을 저장했습니다.", false
	case "saved-unlinked":
		return "새 Discord 애플리케이션 설정을 저장했습니다. 사용할 계정을 다시 연동해 주세요.", false
	case "linked":
		return "Discord 계정 연동을 완료했습니다. 이제 해당 계정으로만 로그인할 수 있습니다.", false
	case "unlinked":
		return "Discord 계정 연동을 해제했습니다. Discord 로그인은 다시 연동할 때까지 거부됩니다.", false
	case "invalid-settings":
		return "입력값을 확인해 주세요. Client ID, Client Secret, 콜백 URL이 모두 필요합니다.", true
	case "invalid-redirect":
		return "콜백 URL은 HTTPS 주소여야 하며 경로는 /ra/discord/callback 이어야 합니다. 로컬 개발에서는 HTTP loopback 주소를 사용할 수 있습니다.", true
	case "save-failed":
		return "Discord OAuth 설정을 데이터베이스에 저장하지 못했습니다.", true
	case "oauth-error":
		return "Discord 인증을 완료하지 못했습니다. 설정값과 Discord Developer Portal의 Redirect URI를 확인해 주세요.", true
	default:
		return "", false
	}
}

func (a *app) adminDiscordSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminAuthenticated(r) {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
		return
	}
	if !a.validateCSRF(r) {
		http.Error(w, "CSRF 토큰이 유효하지 않습니다.", http.StatusForbidden)
		return
	}

	current, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		http.Redirect(w, r, "/ra/discord?status=save-failed", http.StatusSeeOther)
		return
	}
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	clientSecret := strings.TrimSpace(r.FormValue("client_secret"))
	redirectURI := strings.TrimSpace(r.FormValue("redirect_uri"))
	if clientSecret == "" {
		clientSecret = current.ClientSecret
	}
	if !discordSnowflakePattern.MatchString(clientID) || clientSecret == "" || len(clientSecret) > 512 {
		http.Redirect(w, r, "/ra/discord?status=invalid-settings", http.StatusSeeOther)
		return
	}
	if err := validateDiscordRedirectURI(redirectURI); err != nil {
		http.Redirect(w, r, "/ra/discord?status=invalid-redirect", http.StatusSeeOther)
		return
	}

	next := current
	next.ClientID = clientID
	next.ClientSecret = clientSecret
	next.RedirectURI = redirectURI
	status := "saved"
	if current.ClientID != "" && current.ClientID != clientID && current.linked() {
		next.LinkedUserID = ""
		next.LinkedUsername = ""
		next.LinkedGlobalName = ""
		next.LinkedAvatar = ""
		status = "saved-unlinked"
	}
	if err := a.saveDiscordOAuthConfig(next); err != nil {
		log.Printf("Discord OAuth 설정 저장 실패: %v", err)
		http.Redirect(w, r, "/ra/discord?status=save-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ra/discord?status="+status, http.StatusSeeOther)
}

func validateDiscordRedirectURI(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid redirect URI")
	}
	if parsed.Path != "/ra/discord/callback" {
		return errors.New("unexpected callback path")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	hostname := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1") {
		return nil
	}
	return errors.New("HTTPS required")
}

func (a *app) adminDiscordLinkHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminAuthenticated(r) {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
		return
	}
	a.beginDiscordOAuth(w, r, discordOAuthIntentLink)
}

func (a *app) discordLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		a.renderAdminLogin(w, http.StatusServiceUnavailable, "Discord OAuth 설정을 데이터베이스에서 불러오지 못했습니다.")
		return
	}
	if !cfg.ready() {
		a.renderAdminLogin(w, http.StatusForbidden, "관리자가 Discord 계정을 연동하지 않아 Discord 로그인을 사용할 수 없습니다.")
		return
	}
	a.beginDiscordOAuth(w, r, discordOAuthIntentLogin)
}

func (a *app) beginDiscordOAuth(w http.ResponseWriter, r *http.Request, intent string) {
	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		a.handleDiscordOAuthFailure(w, r, intent, "Discord OAuth 설정을 불러오지 못했습니다.")
		return
	}
	if !cfg.configured() || (intent == discordOAuthIntentLogin && !cfg.linked()) {
		a.handleDiscordOAuthFailure(w, r, intent, "Discord OAuth 설정 또는 계정 연동이 완료되지 않았습니다.")
		return
	}

	state, err := pastebox.RandomString(pastebox.AlphanumericAlphabet, 48)
	if err != nil {
		a.handleDiscordOAuthFailure(w, r, intent, "OAuth 상태값을 생성하지 못했습니다.")
		return
	}
	if a.discordOAuthStore == nil {
		a.handleDiscordOAuthFailure(w, r, intent, "Discord OAuth는 데이터베이스 저장소 모드에서만 사용할 수 있습니다.")
		return
	}
	if err := a.discordOAuthStore.CreateDiscordOAuthState(pastebox.DiscordOAuthPendingState{
		State:     state,
		Intent:    intent,
		ExpiresAt: time.Now().UTC().Add(discordOAuthStateMaxAge),
	}); err != nil {
		log.Printf("Discord OAuth state 저장 실패: %v", err)
		a.handleDiscordOAuthFailure(w, r, intent, "OAuth 상태값을 저장하지 못했습니다.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     discordOAuthStateCookieName,
		Value:    state,
		Path:     "/ra/discord",
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(discordOAuthStateMaxAge.Seconds()),
	})

	params := url.Values{
		"response_type": {"code"},
		"client_id":     {cfg.ClientID},
		"scope":         {"identify"},
		"state":         {state},
		"redirect_uri":  {cfg.RedirectURI},
	}
	if intent == discordOAuthIntentLink {
		params.Set("prompt", "consent")
	}
	http.Redirect(w, r, discordOAuthAuthorizeURL+"?"+params.Encode(), http.StatusFound)
}

func clearDiscordOAuthStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     discordOAuthStateCookieName,
		Value:    "",
		Path:     "/ra/discord",
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (a *app) discordCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	state := strings.TrimSpace(r.URL.Query().Get("state"))
	stateCookie, cookieErr := r.Cookie(discordOAuthStateCookieName)
	if cookieErr != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
		clearDiscordOAuthStateCookie(w, r)
		a.renderAdminLogin(w, http.StatusBadRequest, "Discord 로그인 요청이 만료되었거나 유효하지 않습니다. 다시 시도해 주세요.")
		return
	}
	if a.discordOAuthStore == nil {
		clearDiscordOAuthStateCookie(w, r)
		http.Error(w, "Discord OAuth는 데이터베이스 저장소 모드에서만 사용할 수 있습니다.", http.StatusServiceUnavailable)
		return
	}
	pending, ok, err := a.discordOAuthStore.ConsumeDiscordOAuthState(state, time.Now().UTC())
	clearDiscordOAuthStateCookie(w, r)
	if err != nil {
		log.Printf("Discord OAuth state 조회 실패: %v", err)
		http.Error(w, "Discord OAuth 상태를 데이터베이스에서 확인하지 못했습니다.", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		a.renderAdminLogin(w, http.StatusBadRequest, "Discord 로그인 요청이 만료되었거나 유효하지 않습니다. 다시 시도해 주세요.")
		return
	}
	if pending.Intent == discordOAuthIntentLink && !a.isAdminAuthenticated(r) {
		a.renderAdminLogin(w, http.StatusUnauthorized, "관리자 세션이 만료되었습니다. 관리자 토큰으로 다시 로그인한 뒤 계정을 연동해 주세요.")
		return
	}
	if oauthError := strings.TrimSpace(r.URL.Query().Get("error")); oauthError != "" {
		log.Printf("Discord OAuth 사용자 승인 실패 (intent=%s, error=%s)", pending.Intent, oauthError)
		a.handleDiscordOAuthFailure(w, r, pending.Intent, "Discord 인증이 취소되었거나 승인되지 않았습니다.")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		a.handleDiscordOAuthFailure(w, r, pending.Intent, "Discord가 인증 코드를 반환하지 않았습니다.")
		return
	}

	cfg, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		a.handleDiscordOAuthFailure(w, r, pending.Intent, "Discord OAuth 설정을 불러오지 못했습니다.")
		return
	}
	accessToken, err := a.exchangeDiscordCode(r, cfg, code)
	if err != nil {
		log.Printf("Discord OAuth 코드 교환 실패 (intent=%s): %v", pending.Intent, err)
		a.handleDiscordOAuthFailure(w, r, pending.Intent, "Discord 인증 코드를 확인하지 못했습니다.")
		return
	}
	user, err := a.fetchDiscordUser(r, accessToken)
	if err != nil {
		log.Printf("Discord OAuth 사용자 조회 실패 (intent=%s): %v", pending.Intent, err)
		a.handleDiscordOAuthFailure(w, r, pending.Intent, "Discord 사용자 정보를 확인하지 못했습니다.")
		return
	}

	if pending.Intent == discordOAuthIntentLink {
		next := cfg
		next.LinkedUserID = user.ID
		next.LinkedUsername = user.Username
		next.LinkedGlobalName = user.GlobalName
		next.LinkedAvatar = user.Avatar
		if err := a.saveDiscordOAuthConfig(next); err != nil {
			log.Printf("Discord 연동 계정 저장 실패: %v", err)
			http.Redirect(w, r, "/ra/discord?status=save-failed", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/ra/discord?status=linked", http.StatusSeeOther)
		return
	}

	current, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 재조회 실패: %v", err)
		a.renderAdminLogin(w, http.StatusServiceUnavailable, "Discord OAuth 설정을 확인하지 못했습니다.")
		return
	}
	if !current.ready() || subtle.ConstantTimeCompare([]byte(user.ID), []byte(current.LinkedUserID)) != 1 {
		a.renderAdminLogin(w, http.StatusForbidden, "이 Discord 계정은 관리자에 의해 연동되지 않았습니다.")
		return
	}
	a.setAdminSessionCookie(w, r)
	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) exchangeDiscordCode(r *http.Request, cfg discordOAuthConfig, code string) (string, error) {
	if !cfg.configured() {
		return "", errors.New("Discord OAuth is not configured")
	}
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {cfg.RedirectURI},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, discordOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)

	resp, err := a.discordHTTP().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("Discord token endpoint returned %d", resp.StatusCode)
	}
	var token discordTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", errors.New("Discord returned an invalid access token response")
	}
	return token.AccessToken, nil
}

func (a *app) fetchDiscordUser(r *http.Request, accessToken string) (discordUser, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, discordOAuthUserURL, nil)
	if err != nil {
		return discordUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := a.discordHTTP().Do(req)
	if err != nil {
		return discordUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return discordUser{}, fmt.Errorf("Discord user endpoint returned %d", resp.StatusCode)
	}
	var user discordUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return discordUser{}, err
	}
	if !discordSnowflakePattern.MatchString(user.ID) || strings.TrimSpace(user.Username) == "" {
		return discordUser{}, errors.New("Discord returned an invalid user response")
	}
	return user, nil
}

func (a *app) handleDiscordOAuthFailure(w http.ResponseWriter, r *http.Request, intent, publicMessage string) {
	if intent == discordOAuthIntentLink {
		http.Redirect(w, r, "/ra/discord?status=oauth-error", http.StatusSeeOther)
		return
	}
	a.renderAdminLogin(w, http.StatusUnauthorized, publicMessage)
}

func (a *app) adminDiscordUnlinkHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminAuthenticated(r) {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
		return
	}
	if !a.validateCSRF(r) {
		http.Error(w, "CSRF 토큰이 유효하지 않습니다.", http.StatusForbidden)
		return
	}

	next, err := a.getDiscordOAuthConfig()
	if err != nil {
		log.Printf("Discord OAuth 설정 조회 실패: %v", err)
		http.Redirect(w, r, "/ra/discord?status=save-failed", http.StatusSeeOther)
		return
	}
	next.LinkedUserID = ""
	next.LinkedUsername = ""
	next.LinkedGlobalName = ""
	next.LinkedAvatar = ""
	if err := a.saveDiscordOAuthConfig(next); err != nil {
		log.Printf("Discord 연동 해제 저장 실패: %v", err)
		http.Redirect(w, r, "/ra/discord?status=save-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ra/discord?status=unlinked", http.StatusSeeOther)
}
