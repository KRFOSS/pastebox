package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pastebox "pastebox/internal"
)

const webDeleteNonceField = "web_delete_nonce"
const webDeleteNonceCookiePrefix = "paste_web_delete_"

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	dataDir := getenv("DATA_DIR", "/paste-data")
	expireDays := getenvInt("EXPIRE_DAYS", 30)
	storageMode := "local"
	dbDSN := ""
	dbCompress := "zstd"
	adminToken := ""
	maxUploadSizeMB := int64(10)
	rateLimitPerSec := getenvFloat("RATE_LIMIT_PER_SEC", 2)
	rateBurst := getenvFloat("RATE_LIMIT_BURST", 10)

	cfg, err := loadConfig("config.conf")
	if err == nil {
		if cfg.ListenAddr != "" {
			listenAddr = cfg.ListenAddr
		}
		if cfg.DataDir != "" {
			dataDir = cfg.DataDir
		}
		if cfg.ExpireDays > 0 {
			expireDays = cfg.ExpireDays
		}
		if cfg.StorageMode != "" {
			storageMode = strings.ToLower(cfg.StorageMode)
		}
		dbDSN = cfg.DBDSN
		if cfg.DBCompress != "" {
			dbCompress = cfg.DBCompress
		}
		if err := ensureAdminToken("config.conf", cfg); err != nil {
			log.Printf("ADMIN_TOKEN 파일 기록 실패: %v", err)
		}
		adminToken = cfg.AdminToken
		if cfg.MaxUploadSizeMB > 0 {
			maxUploadSizeMB = cfg.MaxUploadSizeMB
		}
		if cfg.RateLimitPerSec > 0 {
			rateLimitPerSec = cfg.RateLimitPerSec
		}
		if cfg.RateBurst > 0 {
			rateBurst = cfg.RateBurst
		}
		log.Println("설정 파일(config.conf)이 성공적으로 로드되었습니다.")
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("설정 파일(config.conf) 로드 실패 (환경 변수 모드로 실행): %v", err)
		} else {
			log.Println("설정 파일이 발견되지 않아 환경 변수 기반으로 구동합니다.")
		}
	}

	var store pastebox.Storage
	if storageMode == "db" {
		if dbDSN == "" {
			log.Fatal("DB 모드 실행을 위해 config.conf 내 DB_DSN 설정이 필요합니다.")
		}
		log.Printf("DB 모드로 시작합니다. DSN=%s, 압축=%s", dbDSN, dbCompress)
		store, err = pastebox.NewDBStore(dbDSN, time.Duration(expireDays)*24*time.Hour, dbCompress)
		if err != nil {
			log.Fatalf("DB 연결 및 초기화 실패: %v", err)
		}
	} else {
		log.Printf("로컬 스토리지 모드로 시작합니다. 경로=%s", dataDir)
		store, err = pastebox.NewLocalStore(dataDir, time.Duration(expireDays)*24*time.Hour)
		if err != nil {
			log.Fatalf("로컬 스토리지 초기화 실패: %v", err)
		}
	}

	indexTmpl, pasteTmpl, passwordTmpl, adminLoginTmpl, adminDashTmpl := loadTemplates()

	a := &app{
		store:               store,
		index:               indexTmpl,
		pasteView:           pasteTmpl,
		password:            passwordTmpl,
		adminLogin:          adminLoginTmpl,
		adminDashboard:      adminDashTmpl,
		adminToken:          adminToken,
		expireDays:          expireDays,
		maxUploadSize:       maxUploadSizeMB * 1024 * 1024,
		homeBackgroundImage: cfg.HomeBackgroundImage,
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			if err := store.CleanupExpired(); err != nil {
				log.Printf("만료 항목 정리 실패: %v", err)
			}
			<-ticker.C
		}
	}()

	uploadLimiter := newRateLimiter(rateLimitPerSec, rateBurst)

	mux := http.NewServeMux()
	mux.HandleFunc("/", uploadLimiter.middleware(a.handle))
	mux.HandleFunc("/ra", a.adminHandler)
	mux.HandleFunc("/ra/login", uploadLimiter.middleware(a.adminLoginHandler))
	mux.HandleFunc("/ra/logout", a.adminLogoutHandler)
	mux.HandleFunc("/ra/delete", a.adminDeleteHandler)
	mux.HandleFunc("/ra/delete-all", a.adminDeleteAllHandler)
	mux.HandleFunc("/ra/limit", a.adminUpdateLimitHandler)
	mux.HandleFunc("/ra/home-bg", a.adminUpdateHomeBgHandler)

	log.Printf("서버가 %s 주소에서 대기 중입니다", listenAddr)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      webDeleteProtection(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("서버 시작 실패: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("수신된 시그널 %v, 서버를 안전하게 종료합니다...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("서버 종료 중 오류: %v", err)
	}

	if err := store.Close(); err != nil {
		log.Printf("스토리지 종료 중 오류: %v", err)
	}

	log.Println("서버가 안전하게 종료되었습니다.")
}

func webDeleteProtection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("delete") == "force" {
			if !validateWebDeleteRequest(w, r) {
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		id, ok := webDeleteIDFromPath(r.URL.Path)
		if !ok || r.Method != http.MethodGet || r.URL.Query().Get("raw") == "1" || r.URL.Query().Get("password") != "" {
			next.ServeHTTP(w, r)
			return
		}

		nonce, err := pastebox.RandomString(pastebox.AlphanumericAlphabet, 32)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		setWebDeleteNonceCookie(w, r, id, nonce)

		buffered := &webDeleteBufferedWriter{ResponseWriter: w}
		next.ServeHTTP(buffered, r)

		statusCode := buffered.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}

		body := buffered.body.Bytes()
		if statusCode == http.StatusOK && strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/html") {
			body = injectWebDeleteFormScript(body, nonce)
			w.Header().Del("Content-Length")
		}

		w.WriteHeader(statusCode)
		_, _ = w.Write(body)
	})
}

func validateWebDeleteRequest(w http.ResponseWriter, r *http.Request) bool {
	id, ok := webDeleteIDFromPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return false
	}

	if r.Method != http.MethodPost {
		http.Error(w, "웹뷰어 삭제는 POST 요청만 허용됩니다.", http.StatusMethodNotAllowed)
		return false
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "유효하지 않은 웹뷰어 삭제 요청입니다.", http.StatusBadRequest)
		return false
	}

	formNonce := strings.TrimSpace(r.FormValue(webDeleteNonceField))
	cookie, err := r.Cookie(webDeleteNonceCookieName(id))
	if err != nil || formNonce == "" || subtle.ConstantTimeCompare([]byte(formNonce), []byte(cookie.Value)) != 1 {
		http.Error(w, "웹뷰어 삭제 요청 검증에 실패했습니다.", http.StatusForbidden)
		return false
	}

	clearWebDeleteNonceCookie(w, r, id)
	return true
}

func setWebDeleteNonceCookie(w http.ResponseWriter, r *http.Request, id string, nonce string) {
	http.SetCookie(w, &http.Cookie{
		Name:     webDeleteNonceCookieName(id),
		Value:    nonce,
		Path:     "/" + id,
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   3600,
	})
}

func clearWebDeleteNonceCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     webDeleteNonceCookieName(id),
		Value:    "",
		Path:     "/" + id,
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func webDeleteNonceCookieName(id string) string {
	return webDeleteNonceCookiePrefix + id
}

func webDeleteIDFromPath(path string) (string, bool) {
	id := strings.TrimPrefix(path, "/")
	if id == "" || strings.Contains(id, "/") || len(id) != 5 {
		return "", false
	}

	for _, r := range id {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		return "", false
	}

	return id, true
}

func isHTTPSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		return false
	}

	proto = strings.TrimSpace(strings.Split(proto, ",")[0])
	return strings.EqualFold(proto, "https")
}

func injectWebDeleteFormScript(body []byte, nonce string) []byte {
	script := fmt.Sprintf(`<script>
(function () {
    var deleteLink = document.querySelector('a[href="?delete=force"]');
    if (!deleteLink) {
        return;
    }
    deleteLink.addEventListener('click', function (event) {
        event.preventDefault();
        var form = document.createElement('form');
        form.method = 'post';
        form.action = '?delete=force';
        var input = document.createElement('input');
        input.type = 'hidden';
        input.name = '%s';
        input.value = '%s';
        form.appendChild(input);
        document.body.appendChild(form);
        form.submit();
    });
})();
</script>`, webDeleteNonceField, nonce)

	return bytes.Replace(body, []byte("</body>"), []byte(script+"\n</body>"), 1)
}

type webDeleteBufferedWriter struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
}

func (w *webDeleteBufferedWriter) WriteHeader(statusCode int) {
	if w.statusCode == 0 {
		w.statusCode = statusCode
	}
}

func (w *webDeleteBufferedWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.body.Write(p)
}
