package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pastebox "pastebox/internal"
)

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
		store:          store,
		index:          indexTmpl,
		pasteView:      pasteTmpl,
		password:       passwordTmpl,
		adminLogin:     adminLoginTmpl,
		adminDashboard: adminDashTmpl,
		adminToken:     adminToken,
		expireDays:     expireDays,
		maxUploadSize:  maxUploadSizeMB * 1024 * 1024,
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

	log.Printf("서버가 %s 주소에서 대기 중입니다", listenAddr)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
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
