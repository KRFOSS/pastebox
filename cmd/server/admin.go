package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (a *app) adminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_token")
	if err != nil || cookie.Value != a.adminToken || a.adminToken == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = a.adminLogin.Execute(w, map[string]any{
			"Error": "",
		})
		return
	}

	pastes, err := a.store.List()
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.adminDashboard.Execute(w, map[string]any{
		"Pastes":         pastes,
		"StorageMode":    a.getStorageModeString(),
		"CurrentLimitMB": a.getMaxUploadSize() / (1024 * 1024),
	})
}

func (a *app) adminUpdateLimitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_token")
	if err != nil || cookie.Value != a.adminToken || a.adminToken == "" {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
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

	a.mu.Lock()
	a.maxUploadSize = newMaxBytes
	a.mu.Unlock()

	cfgData, err := os.ReadFile("config.conf")
	if err == nil {
		lines := strings.Split(string(cfgData), "\n")
		found := false
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(line)), "MAX_UPLOAD_SIZE_MB=") {
				lines[i] = fmt.Sprintf("MAX_UPLOAD_SIZE_MB=%d", newMaxMB)
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, fmt.Sprintf("MAX_UPLOAD_SIZE_MB=%d", newMaxMB))
		}
		_ = os.WriteFile("config.conf", []byte(strings.Join(lines, "\n")), 0644)
	}

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	if a.adminToken == "" || token != a.adminToken {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.adminLogin.Execute(w, map[string]any{
			"Error": "입력하신 토큰이 일치하지 않거나 토큰 정보가 비어있습니다.",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "admin_token",
		Value:    token,
		Path:     "/ra",
		HttpOnly: true,
		MaxAge:   86400,
	})

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_token",
		Value:    "",
		Path:     "/ra",
		HttpOnly: true,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}

func (a *app) adminDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_token")
	if err != nil || cookie.Value != a.adminToken || a.adminToken == "" {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
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

	cookie, err := r.Cookie("admin_token")
	if err != nil || cookie.Value != a.adminToken || a.adminToken == "" {
		http.Error(w, "권한이 없습니다.", http.StatusUnauthorized)
		return
	}

	if err := a.store.DeleteAll(); err != nil {
		log.Printf("전체 삭제 실패: %v", err)
		http.Error(w, "전체 삭제 중 오류가 발생했습니다.", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/ra", http.StatusSeeOther)
}
