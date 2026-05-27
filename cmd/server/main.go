package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
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
	defer store.Close()

	indexTemplate, pasteTemplate, adminLoginTemplate, adminDashboardTemplate := loadTemplates()

	a := &app{
		store:          store,
		index:          indexTemplate,
		pasteView:      pasteTemplate,
		adminLogin:     adminLoginTemplate,
		adminDashboard: adminDashboardTemplate,
		adminToken:     adminToken,
		expireDays:     expireDays,
		maxUploadSize:  maxUploadSizeMB * 1024 * 1024,
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			if err := store.CleanupExpired(); err != nil {
				log.Printf("cleanup failed: %v", err)
			}
			<-ticker.C
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handle)
	mux.HandleFunc("/ra", a.adminHandler)
	mux.HandleFunc("/ra/login", a.adminLoginHandler)
	mux.HandleFunc("/ra/logout", a.adminLogoutHandler)
	mux.HandleFunc("/ra/delete", a.adminDeleteHandler)
	mux.HandleFunc("/ra/delete-all", a.adminDeleteAllHandler)
	mux.HandleFunc("/ra/limit", a.adminUpdateLimitHandler)

	log.Printf("pastebox listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
