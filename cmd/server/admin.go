package main

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	pastebox "pastebox/internal"
)

// isAdminAuthenticated는 관리자 쿠키의 토큰을 상수 시간으로 비교하여 인증 여부를 반환합니다.
func (a *app) isAdminAuthenticated(r *http.Request) bool {
	if a.adminToken == "" {
		return false
	}
	cookie, err := r.Cookie("admin_token")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(a.adminToken)) == 1
}

// setCSRFCookie는 CSRF 토큰을 생성하여 쿠키에 설정하고 토큰 값을 반환합니다.
func (a *app) setCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	token, err := pastebox.RandomString(pastebox.AlphanumericAlphabet, 32)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    token,
		Path:     "/ra",
		HttpOnly: false,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   3600,
	})
	return token
}

// validateCSRF는 폼에서 제출된 CSRF 토큰을 쿠키의 토큰과 상수 시간으로 비교합니다.
func (a *app) validateCSRF(r *http.Request) bool {
	formToken := r.FormValue("csrf_token")
	if formToken == "" {
		return false
	}
	cookie, err := r.Cookie("csrf_token")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(formToken), []byte(cookie.Value)) == 1
}

func (a *app) adminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	if !a.isAdminAuthenticated(r) {
		a.renderAdminLogin(w, http.StatusOK, "")
		return
	}

	// Pagination & Sorting Parameters
	pageStr := r.URL.Query().Get("page")
	limitStr := r.URL.Query().Get("limit")
	sortBy := r.URL.Query().Get("sort")
	order := r.URL.Query().Get("order")

	if sortBy == "" {
		sortBy = "created_at"
	}
	if order == "" {
		order = "desc"
	}

	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 1 {
		limit = 15 // Default
	}
	if limit > 100 {
		limit = 100 // Max 100
	}

	offset := (page - 1) * limit

	pastes, totalCount, err := a.store.List(sortBy, order, offset, limit)
	if err != nil {
		http.Error(w, "데이터 조회 실패", http.StatusInternalServerError)
		return
	}

	for i := range pastes {
		if pastes[i].ExpiresAt.IsZero() {
			ttl := time.Duration(a.expireDays) * 24 * time.Hour
			if ttl <= 0 {
				ttl = 30 * 24 * time.Hour
			}
			pastes[i].ExpiresAt = pastes[i].CreatedAt.Add(ttl)
		}
	}

	totalPages := totalCount / limit
	if totalCount%limit != 0 {
		totalPages++
	}
	if totalPages == 0 {
		totalPages = 1
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	csrfToken := a.setCSRFCookie(w, r)
	_ = a.adminDashboard.Execute(w, map[string]any{
		"Pastes":         pastes,
		"StorageMode":    a.getStorageModeString(),
		"CurrentLimitMB": a.getMaxUploadSize() / (1024 * 1024),
		"ExpireDays":     a.expireDays,
		"CSRFToken":      csrfToken,
		"TotalCount":     totalCount,
		"TotalPages":     totalPages,
		"CurrentPage":    page,
		"PrevPage":       page - 1,
		"NextPage":       page + 1,
		"Limit":          limit,
		"SortBy":         sortBy,
		"Order":          order,
		"HomeBgUrl":      a.getHomeBackgroundImage(),
	})
}

func (a *app) adminUpdateLimitHandler(w http.ResponseWriter, r *http.Request) {
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

	sizeStr := r.FormValue("size")
	unit := r.FormValue("unit")

	size, err := strconv.ParseFloat(sizeStr, 64)
	if err != nil || size <= 0 {
		http.Error(w, "올바른 용량을 입력하세요.", http.StatusBadRequest)
		return
	}

	var multiplier float64
	switch unit {
	case "KB":
		multiplier = 1024
	case "MB":
		multiplier = 1024 * 1024
	case "GB":
		multiplier = 1024 * 1024 * 1024
	default:
		multiplier = 1024 * 1024
	}

	newMaxBytes := int64(size * multiplier)
	newMaxMB := newMaxBytes / (1024 * 1024)
	if newMaxMB < 1 {
		newMaxMB = 1
	}

	if err := persistConfigValues(a.configFilePath(), map[string]string{
		"MAX_UPLOAD_SIZE_MB": strconv.FormatInt(newMaxMB, 10),
	}); err != nil {
		log.Printf("최대 업로드 용량 설정 저장 실패: %v", err)
		http.Error(w, "설정 파일을 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}

	a.mu.Lock()
	a.maxUploadSize = newMaxBytes
	a.mu.Unlock()

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	if a.adminToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.adminToken)) != 1 {
		a.renderAdminLogin(w, http.StatusUnauthorized, "입력하신 토큰이 일치하지 않거나 토큰 정보가 비어있습니다.")
		return
	}

	a.setAdminSessionCookie(w, r)

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_token",
		Value:    "",
		Path:     "/ra",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminDeleteHandler(w http.ResponseWriter, r *http.Request) {
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

	idsStr := r.FormValue("ids")
	if idsStr == "" {
		http.Redirect(w, r, "/ra", http.StatusSeeOther)
		return
	}

	ids := strings.Split(idsStr, ",")
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			if err := a.store.ForceDelete(id); err != nil {
				log.Printf("관리자 강제 삭제 실패 (ID=%s): %v", id, err)
			}
		}
	}

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminDeleteAllHandler(w http.ResponseWriter, r *http.Request) {
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

	if err := a.store.DeleteAll(); err != nil {
		log.Printf("전체 삭제 실패: %v", err)
		http.Error(w, "전체 삭제 중 오류가 발생했습니다.", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminUpdateHomeBgHandler(w http.ResponseWriter, r *http.Request) {
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

	bgUrl := r.FormValue("bg_url")

	if err := persistConfigValues(a.configFilePath(), map[string]string{
		"HOME_BACKGROUND_IMAGE_URL": bgUrl,
	}); err != nil {
		log.Printf("홈 배경 이미지 설정 저장 실패: %v", err)
		http.Error(w, "설정 파일을 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}

	a.mu.Lock()
	a.homeBackgroundImage = bgUrl
	a.mu.Unlock()

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}
