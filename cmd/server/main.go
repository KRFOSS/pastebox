package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	pastebox "pastebox/internal"
)

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

type config struct {
	StorageMode string
	ListenAddr  string
	DataDir     string
	ExpireDays  int
	DBDSN       string
	DBCompress  string
	AdminToken  string
}

func loadConfig(path string) (*config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &config{
		StorageMode: "local",
		ListenAddr:  ":8080",
		DataDir:     "./data",
		ExpireDays:  30,
		DBCompress:  "zstd",
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch key {
		case "STORAGE_MODE":
			cfg.StorageMode = val
		case "LISTEN_ADDR":
			cfg.ListenAddr = val
		case "DATA_DIR":
			cfg.DataDir = val
		case "EXPIRE_DAYS":
			if days, err := strconv.Atoi(val); err == nil {
				cfg.ExpireDays = days
			}
		case "DB_DSN":
			cfg.DBDSN = val
		case "DB_COMPRESSION_ALGORITHM":
			cfg.DBCompress = val
		case "ADMIN_TOKEN":
			cfg.AdminToken = val
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cfg, nil
}

type app struct {
	store      pastebox.Storage
	index      *template.Template
	adminToken string
	expireDays int
}

func main() {
	// 기본값 설정
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	dataDir := getenv("DATA_DIR", "/paste-data")
	expireDays := getenvInt("EXPIRE_DAYS", 30)
	storageMode := "local"
	dbDSN := ""
	dbCompress := "zstd"
	adminToken := ""

	// config.conf 로드 시도
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

		indexTemplate, err := template.ParseFiles("templates/index.html")
	if err != nil {
		indexTemplate = template.Must(template.New("index").Parse(fallbackIndexHTML))
	}

	a := &app{
		store:      store,
		index:      indexTemplate,
		adminToken: adminToken,
		expireDays: expireDays,
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

	log.Printf("pastebox listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/robots.txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/temp" {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path == "/temp" {
				http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
				return
			}
			a.indexHandler(w, r)
		case http.MethodPost, http.MethodPut:
			if r.URL.Path == "/temp" {
				r.Header.Set("data-policy", "once")
			}
			a.uploadHandler(w, r)
		default:
			http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		}
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(id, "/") || id == "" {
		http.NotFound(w, r)
		return
	}

	if token := r.URL.Query().Get("delete"); token != "" {
		a.deleteHandler(w, r, id, token)
		return
	}

	a.viewHandler(w, r, id)
}

func (a *app) indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := map[string]string{
		"BaseURL": requestBaseURL(r),
	}

	if err := a.index.Execute(w, data); err != nil {
		http.Error(w, "템플릿 에러", http.StatusInternalServerError)
	}
}

func (a *app) uploadHandler(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 10 << 20 // 10MB limit
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	var reader io.Reader
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			http.Error(w, "업로드 용량(10MB) 초과 또는 유효하지 않은 요청입니다.", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file 필드가 누락되었습니다.", http.StatusBadRequest)
			return
		}
		defer file.Close()

		reader = file

		if header != nil {
			if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); detected != "" {
				contentType = detected
			} else {
				contentType = "application/octet-stream"
			}
		}
	} else {
		reader = r.Body
		if strings.TrimSpace(contentType) == "" {
			contentType = "text/plain; charset=utf-8"
		}
	}

	buf := make([]byte, 4096)
	n, err := io.ReadFull(reader, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		http.Error(w, "파일 읽기 중 오류 발생", http.StatusInternalServerError)
		return
	}

	isText := false
	if n == 0 {
		isText = true
	} else {
		detected := http.DetectContentType(buf[:n])
		if strings.HasPrefix(detected, "text/") || strings.Contains(detected, "json") {
			isText = true
		} else {
			isText = looksLikeText(buf[:n])
		}
	}

	if !isText {
		http.Error(w, "바이너리 파일은 공유할 수 없습니다. 텍스트/로그 데이터만 업로드 가능합니다.", http.StatusBadRequest)
		return
	}

	reader = io.MultiReader(bytes.NewReader(buf[:n]), reader)

	usePassword := strings.EqualFold(strings.TrimSpace(r.Header.Get("usepassword")), "true")
	
	policy := r.Header.Get("data-policy")
	if policy == "" {
		policy = r.FormValue("data-policy")
	}
	once := strings.EqualFold(strings.TrimSpace(policy), "once")

	meta, password, deleteToken, err := a.store.Create(reader, contentType, usePassword, once)
	if err != nil {
		log.Printf("upload failed: %v", err)
		
		var maxBytesError *http.MaxBytesError
		isTooLarge := false
		if errors.As(err, &maxBytesError) {
			isTooLarge = true
		} else if strings.Contains(err.Error(), "request body too large") || strings.Contains(err.Error(), "MaxBytesError") || strings.Contains(err.Error(), "exceeds maximum") {
			isTooLarge = true
		}
		
		if isTooLarge {
			maxMB := float64(maxUploadSize) / (1024 * 1024)
			if r.ContentLength > 0 {
				reqMB := float64(r.ContentLength) / (1024 * 1024)
				http.Error(w, fmt.Sprintf("업로드 실패: 현재 %.1fMB 크기의 업로드를 시도했습니다.\n정책상 %.1fMB까지만 업로드가 허용됩니다.", reqMB, maxMB), http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, fmt.Sprintf("업로드 실패: 허용된 최대 용량(%.1fMB)을 초과했습니다.", maxMB), http.StatusRequestEntityTooLarge)
			}
			return
		}

		http.Error(w, "업로드 실패", http.StatusInternalServerError)
		return
	}

	url := strings.TrimRight(requestBaseURL(r), "/") + "/" + meta.ID

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "주소: %s\n", url)

	if !meta.ExpiresAt.IsZero() {
		fmt.Fprintf(w, "만료일: %s\n", meta.ExpiresAt.Format(time.RFC3339))
	}

	if password != "" {
		fmt.Fprintf(w, "비밀번호: %s\n", password)
	}

	fmt.Fprintf(w, "삭제링크: %s?delete=%s\n", url, deleteToken)
}

func (a *app) deleteHandler(w http.ResponseWriter, r *http.Request, id string, token string) {
	if r.Method == http.MethodHead {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	if err := a.store.Delete(id, token); err != nil {
		if errors.Is(err, pastebox.ErrInvalidDeleteToken) {
			log.Printf("delete denied: id=%s remote=%s", id, r.RemoteAddr)
			http.Error(w, "삭제 토큰이 누락되었거나 유효하지 않습니다.", http.StatusUnauthorized)
			return
		}

		http.NotFound(w, r)
		return
	}

	log.Printf("deleted: id=%s remote=%s", id, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "삭제되었습니다.")
}

func (a *app) viewHandler(w http.ResponseWriter, r *http.Request, id string) {
	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("paste-password")
	}

	entry, err := a.store.Open(id, password)
	if err != nil {
		if errors.Is(err, pastebox.ErrInvalidPassword) {
			http.Error(w, "비밀번호가 필요하거나 유효하지 않습니다. ?password=... 쿼리 파라미터나 paste-password 헤더를 사용하세요.", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer entry.File.Close()

	raw := r.URL.Query().Get("raw") == "1"
	browser := isBrowserRequest(r)

	if !raw && browser && isTextEntry(entry) {
		content, err := io.ReadAll(entry.File)
		if err != nil {
			http.Error(w, "파일 읽기 실패", http.StatusInternalServerError)
			return
		}

		cleanContent := ansiEscapeRegex.ReplaceAllString(string(content), "")

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_ = pasteViewHTML.Execute(w, map[string]any{
			"ID":      entry.Meta.ID,
			"Content": cleanContent,
		})
		return
	}

	contentType := entry.Meta.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if isTextEntry(entry) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if browser {
			content, err := io.ReadAll(entry.File)
			if err != nil {
				http.Error(w, "파일 읽기 실패", http.StatusInternalServerError)
				return
			}
			cleanContent := ansiEscapeRegex.ReplaceAllString(string(content), "")
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(cleanContent))
			}
			return
		}
	} else {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Meta.ID))
	}

	if r.Method == http.MethodHead {
		return
	}

	_, _ = io.Copy(w, entry.File)
}

func isBrowserRequest(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if strings.HasPrefix(ua, "curl/") || strings.Contains(ua, "wget/") || strings.Contains(ua, "httpie/") {
		return false
	}

	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/html") || accept == ""
}

func isTextEntry(entry *pastebox.Entry) bool {
	contentType := strings.ToLower(entry.Meta.ContentType)
	if strings.HasPrefix(contentType, "text/") {
		return true
	}

	if strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "yaml") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "x-sh") {
		return true
	}

	pos, _ := entry.File.Seek(0, io.SeekCurrent)

	buf := make([]byte, 4096)
	n, _ := entry.File.Read(buf)
	_, _ = entry.File.Seek(pos, io.SeekStart)

	if n == 0 {
		return true
	}

	detected := http.DetectContentType(buf[:n])
	if strings.HasPrefix(detected, "text/") {
		return true
	}
	if strings.Contains(detected, "json") {
		return true
	}

	return looksLikeText(buf[:n])
}

func looksLikeText(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}

	if bytes.IndexByte(buf, 0) >= 0 {
		return false
	}

	bad := 0
	for _, b := range buf {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			bad++
		}
	}

	// Allow up to 3% control characters (e.g., ANSI color codes in logs)
	return bad*100 <= len(buf)*3
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	host := r.Host

	if r.TLS != nil {
		scheme = "https"
	}

	if forwarded := r.Header.Get("Forwarded"); forwarded != "" {
		parts := strings.Split(forwarded, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "proto=") {
				scheme = strings.Trim(strings.TrimPrefix(part, "proto="), `"`)
			}
			if strings.HasPrefix(strings.ToLower(part), "host=") {
				host = strings.Trim(strings.TrimPrefix(part, "host="), `"`)
			}
		}
	}

	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
		scheme = strings.TrimSpace(scheme)
	}

	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = strings.Split(forwardedHost, ",")[0]
		host = strings.TrimSpace(host)
	}

	if host == "" {
		host = "localhost"
	}

	return scheme + "://" + host
}

func getenv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return n
}

var pasteViewHTML = template.Must(template.New("paste").Parse(`<!DOCTYPE html>
<html lang="ko">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{ .ID }} - Pastebox</title>
    <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.11.1/styles/github-dark.min.css">
    <script src="https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.11.1/highlight.min.js"></script>
    <style>
        :root {
            --focus-ring: #06b6d4;
        }

        html {
            box-sizing: border-box;
        }

        *, *::before, *::after {
            box-sizing: inherit;
        }

        html, body {
            width: 100%;
            overflow-x: hidden;
        }

        * {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", sans-serif;
        }

        body {
            background: #1a1a1a;
            color: #e5e5e5;
            margin: 0;
            padding: 0;
            position: relative;
            min-height: 100vh;
            display: flex;
            flex-direction: column;
        }

        /* Navbar (ROKFOSS Main Style) */
        .navbar {
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            z-index: 1000;
            background: linear-gradient( to bottom, rgba(0, 0, 0, 0.8) 0%, rgba(0, 0, 0, 0.65) 20%, rgba(0, 0, 0, 0.50) 40%, rgba(0, 0, 0, 0.35) 60%, rgba(0, 0, 0, 0.20) 75%, rgba(0, 0, 0, 0.10) 85%, rgba(0, 0, 0, 0.04) 93%, rgba(0, 0, 0, 0) 100% ) !important;
            padding: 23px !important;
        }

        .navbar .container {
            display: flex;
            justify-content: space-between;
            align-items: center;
            width: 100%;
            max-width: 1200px;
            margin: 0 auto;
            padding: 0 1rem;
        }

        .navbar-brand img {
            height: 40px;
        }

        .main-container {
            flex: 1;
            display: flex;
            flex-direction: column;
            justify-content: center;
            align-items: center;
            padding: 100px 32px 32px;
            box-sizing: border-box;
        }

        .content-wrapper {
            max-width: 100%;
            width: 100%;
        }

        .header-section {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 24px;
        }

        h1 {
            font-size: 28px;
            font-weight: 700;
            color: #fff;
            margin: 0;
        }

        .actions {
            display: flex;
            gap: 12px;
        }

        .button {
            background-color: #242424;
            color: #ffffff;
            border: 1px solid rgba(84, 84, 88, 0.4);
            border-radius: 10px;
            padding: 10px 18px;
            font-size: 14px;
            font-weight: 600;
            text-decoration: none;
            display: inline-flex;
            align-items: center;
            justify-content: center;
            cursor: pointer;
            transition: background-color 0.2s, transform 0.1s;
            user-select: none;
        }

        .button:hover {
            background-color: #2c2c2e;
        }

        .button:active {
            transform: scale(0.98);
        }

        .button-primary {
            background-color: #ffffff;
            color: #000000;
            border: none;
        }

        .button-primary:hover {
            background-color: rgba(255, 255, 255, 0.85);
        }

        .viewer-container {
            display: flex;
            background-color: #242424;
            border-radius: 10px;
            border: 1px solid rgba(84, 84, 88, 0.2);
            overflow: hidden;
            height: calc(100vh - 200px);
            font-family: ui-monospace, SFMono-Regular, SF Pro Icons, "SF Mono", Menlo, Monaco, Consolas, monospace;
            font-size: 14px;
            line-height: 22px; /* Fixed line height */
            position: relative;
        }

        .line-numbers {
            background-color: #1e1e1e;
            color: #858585;
            user-select: none;
            border-right: 1px solid rgba(84, 84, 88, 0.2);
            width: 60px;
            box-sizing: border-box;
            overflow: hidden;
            position: relative;
        }

        .line-numbers-viewport {
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            padding: 20px 10px 20px 20px;
            white-space: pre;
            text-align: right;
        }

        .code-area {
            flex: 1;
            overflow: auto;
            position: relative;
            height: 100%;
        }

        .spacer {
            width: 1px;
        }

        .content-viewport {
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            padding: 20px;
            color: #ffffff;
            white-space: pre;
            pointer-events: auto;
            tab-size: 4;
            -moz-tab-size: 4;
        }

        .footer-text {
            font-size: 14px;
            color: #aaa;
            margin-top: 2rem;
            text-align: center;
        }

        @media (max-width: 768px) {
            .navbar {
                padding: 12px 15px !important;
            }

            .navbar-brand img {
                height: 28px;
            }

            .main-container {
                padding-top: 120px;
            }

            .header-section {
                flex-direction: column;
                align-items: flex-start;
                gap: 16px;
            }

            .actions {
                width: 100%;
            }

            .button {
                flex: 1;
                width: 100%;
            }
        }
        .modal-overlay {
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: rgba(0, 0, 0, 0.6);
            backdrop-filter: blur(4px);
            -webkit-backdrop-filter: blur(4px);
            z-index: 2000;
            display: none;
            justify-content: center;
            align-items: center;
            opacity: 0;
            transition: opacity 0.3s ease;
        }

        .modal-overlay.active {
            display: flex;
            opacity: 1;
        }

        .modal-content {
            background: #242424;
            border: 1px solid rgba(84, 84, 88, 0.4);
            border-radius: 16px;
            width: 90%;
            max-width: 500px;
            padding: 32px;
            box-shadow: 0 20px 40px rgba(0, 0, 0, 0.4);
            transform: scale(0.95);
            transition: transform 0.3s cubic-bezier(0.175, 0.885, 0.32, 1.275);
            text-align: left;
        }

        .modal-overlay.active .modal-content {
            transform: scale(1);
        }

        .modal-title {
            font-size: 20px;
            font-weight: 700;
            color: #fff;
            margin-bottom: 16px;
            margin-top: 0;
        }

        .modal-body {
            font-size: 14px;
            line-height: 1.6;
            color: rgba(235, 235, 245, 0.8);
            margin-bottom: 32px;
        }

        .modal-close {
            width: 100%;
        }
    </style>
</head>
<body>
    <nav class="navbar">
        <div class="container">
            <a class="navbar-brand" href="/">
                <img data-cfasync="false" loading="eager" decoding="async" src="https://cdn.krfoss.org/web/ROKFOSS.png" alt="ROKFOSS">
            </a>
        </div>
    </nav>

    <main id="main" class="main-container" tabindex="-1">
        <div class="content-wrapper">
            <div class="header-section">
                <h1>Pastebox / {{ .ID }}</h1>
                <div class="actions">
                    <button
                        id="copyButton"
                        type="button"
                        class="button button-primary"
                        onclick="copyPasteContent()"
                    >
                        복사
                    </button>
                    <a class="button" href="?raw=1">원본</a>
                </div>
            </div>
            <div class="viewer-container" id="viewerContainer">
                <div class="line-numbers">
                    <div class="line-numbers-viewport" id="lineNumbers"></div>
                </div>
                <div class="code-area" id="codeArea">
                    <div class="spacer" id="spacer"></div>
                    <div class="content-viewport" id="contentViewport"></div>
                </div>
            </div>
            <div class="footer-text">
                <a href="#" onclick="openToSModal(event)" style="color: inherit; text-decoration: underline; cursor: pointer;">서비스 이용 약관</a>
            </div>
            <div id="pasteData" style="display: none;">{{ .Content }}</div>
        </div>
    </main>

    <!-- ToS Modal -->
    <div class="modal-overlay" id="tosModal" onclick="if(event.target===this) closeToSModal()">
        <div class="modal-content">
            <h2 class="modal-title">서비스 이용 약관</h2>
            <div class="modal-body">
                본 서비스는 임시 텍스트, 코드 및 로그 공유를 목적으로 제공됩니다.<br><br>
                개인 클라우드 스토리지 용도 등 본래 목적에 어긋나는 악의적인 대용량 데이터 업로드 및 서비스 남용 시, 사전 통보 없이 데이터가 즉시 삭제되거나 서비스 이용이 영구적으로 차단될 수 있습니다.
            </div>
            <button class="button modal-close" onclick="closeToSModal()">확인</button>
        </div>
    </div>

    <script>
        function openToSModal(e) {
            e.preventDefault();
            document.getElementById('tosModal').classList.add('active');
        }

        function closeToSModal() {
            document.getElementById('tosModal').classList.remove('active');
        }

        const rawDataEl = document.getElementById('pasteData');
        const rawText = rawDataEl.textContent;
        const lines = rawText.split('\n');

        const container = document.getElementById('viewerContainer');
        const codeArea = document.getElementById('codeArea');
        const spacer = document.getElementById('spacer');
        const viewport = document.getElementById('contentViewport');
        const lineNumbersDiv = document.getElementById('lineNumbers');

        const lineHeight = 22;
        const paddingTop = 20;
        const paddingBottom = 20;

        const totalHeight = lines.length * lineHeight + paddingTop + paddingBottom;
        spacer.style.height = totalHeight + 'px';

        let detectedLanguage = 'plaintext';
        let languageDetected = false;

        function render() {
            const scrollTop = codeArea.scrollTop;
            const containerHeight = codeArea.clientHeight;

            let startIdx = Math.floor((scrollTop - paddingTop) / lineHeight) - 5;
            let endIdx = Math.ceil((scrollTop - paddingTop + containerHeight) / lineHeight) + 5;

            if (startIdx < 0) startIdx = 0;
            if (endIdx > lines.length) endIdx = lines.length;

            const visibleLines = lines.slice(startIdx, endIdx);
            const visibleText = visibleLines.join('\n');
            
            if (typeof hljs !== 'undefined') {
                if (!languageDetected) {
                    const sampleText = lines.slice(0, 100).join('\n');
                    const result = hljs.highlightAuto(sampleText);
                    detectedLanguage = result.language || 'plaintext';
                    languageDetected = true;
                }
                
                if (detectedLanguage !== 'plaintext') {
                    try {
                        viewport.innerHTML = hljs.highlight(visibleText, { language: detectedLanguage, ignoreIllegals: true }).value;
                    } catch (e) {
                        viewport.textContent = visibleText;
                    }
                } else {
                    viewport.textContent = visibleText;
                }
            } else {
                viewport.textContent = visibleText;
            }
            
            const offsetTop = paddingTop + startIdx * lineHeight;
            viewport.style.transform = "translate3d(0, " + offsetTop + "px, 0)";

            let numStr = '';
            for (let i = startIdx + 1; i <= endIdx; i++) {
                numStr += i + '\n';
            }
            lineNumbersDiv.textContent = numStr;
            lineNumbersDiv.style.transform = "translate3d(0, " + (offsetTop - scrollTop) + "px, 0)";
        }

        codeArea.addEventListener('scroll', render);
        window.addEventListener('resize', render);
        render();

        async function copyPasteContent() {
            const button = document.getElementById("copyButton");
            const content = rawText;

            try {
                await navigator.clipboard.writeText(content);
                button.innerText = "복사 완료";
            } catch (error) {
                const textarea = document.createElement("textarea");
                textarea.value = content;
                textarea.setAttribute("readonly", "");
                textarea.style.position = "fixed";
                textarea.style.left = "-9999px";
                document.body.appendChild(textarea);
                textarea.select();
                document.execCommand("copy");
                document.body.removeChild(textarea);
                button.innerText = "복사 완료";
            }

            setTimeout(() => {
                button.innerText = "복사";
            }, 1500);
        }
    </script>
</body>
</html>`))

const fallbackIndexHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Pastebox</title>
    <style>
        :root {
            --focus-ring: #06b6d4;
        }

        html {
            box-sizing: border-box;
        }

        *, *::before, *::after {
            box-sizing: inherit;
        }

        html, body {
            width: 100%;
            overflow-x: hidden;
        }

        * {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "Noto Sans", "Apple Color Emoji", "Segoe UI Emoji", sans-serif;
        }

        body {
            background: #1a1a1a;
            color: #e5e5e5;
            margin: 0;
            padding: 0;
            position: relative;
            min-height: 100vh;
            display: flex;
            flex-direction: column;
        }

        :focus-visible {
            outline: 3px solid var(--focus-ring);
            outline-offset: 2px;
        }

        /* Navbar (ROKFOSS Main Style) */
        .navbar {
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            box-shadow: none !important;
            border: none !important;
            padding-bottom: 40px !important;
            z-index: 1000;
            background: linear-gradient( to bottom, rgba(0, 0, 0, 0.8) 0%, rgba(0, 0, 0, 0.65) 20%, rgba(0, 0, 0, 0.50) 40%, rgba(0, 0, 0, 0.35) 60%, rgba(0, 0, 0, 0.20) 75%, rgba(0, 0, 0, 0.10) 85%, rgba(0, 0, 0, 0.04) 93%, rgba(0, 0, 0, 0) 100% ) !important;
            transition: transform 0.4s cubic-bezier(0.4, 0, 0.2, 1), background 0.4s ease;
            padding: 23px !important;
            transform: translateY(0);
        }

        .navbar .container {
            display: flex;
            justify-content: space-between;
            align-items: center;
            flex-wrap: nowrap;
            width: 100%;
            max-width: 1200px;
            margin: 0 auto;
            padding: 0 1rem;
        }

        .navbar-brand img {
            height: 40px;
        }

        .main-container {
            flex: 1;
            display: flex;
            flex-direction: column;
            justify-content: center;
            align-items: center;
            padding: 140px 20px 80px;
            box-sizing: border-box;
        }

        .content-wrapper {
            max-width: 720px;
            width: 100%;
            text-align: center;
        }

        h1 {
            font-size: 48px;
            font-weight: 700;
            color: #fff;
            margin-bottom: 0.5rem;
        }

        .subtitle {
            font-size: 20px;
            color: #fff;
            margin-bottom: 2rem;
        }

        .description {
            font-size: 16px;
            line-height: 1.5;
            color: #e5e5e5;
            margin-bottom: 2.5rem;
        }

        .info-section {
            background-color: #242424;
            border-radius: 10px;
            padding: 20px;
            margin-bottom: 32px;
            text-align: left;
        }

        .info-item {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 16px 0;
            border-bottom: 0.5px solid rgba(84, 84, 88, 0.4);
        }

        .info-item:last-child {
            border-bottom: none;
            padding-bottom: 0;
        }

        .info-item:first-child {
            padding-top: 0;
        }

        .info-label {
            font-size: 15px;
            line-height: 20px;
            color: rgba(235, 235, 245, 0.6);
            font-weight: 500;
            flex-shrink: 0;
            margin-right: 16px;
        }

        .info-value {
            font-size: 14px;
            line-height: 20px;
            color: #ffffff;
            font-weight: 400;
            text-align: right;
            font-family: ui-monospace, SFMono-Regular, SF Pro Icons, "SF Mono", Menlo, Monaco, Consolas, monospace;
            word-break: break-all;
            white-space: pre-wrap;
            max-width: 75%;
        }

        .actions {
            display: flex;
            gap: 12px;
            margin-top: 24px;
            margin-bottom: 32px;
            justify-content: center;
        }

        .button {
            background-color: #242424;
            color: #ffffff;
            border: 1px solid rgba(84, 84, 88, 0.4);
            border-radius: 10px;
            padding: 12px 24px;
            font-size: 15px;
            font-weight: 600;
            text-decoration: none;
            display: inline-flex;
            align-items: center;
            justify-content: center;
            cursor: pointer;
            transition: background-color 0.2s, transform 0.1s;
            user-select: none;
        }

        .button:hover {
            background-color: #2c2c2e;
        }

        .button:active {
            transform: scale(0.98);
        }

        .button-primary {
            background-color: #ffffff;
            color: #000000;
            border: none;
        }

        .button-primary:hover {
            background-color: rgba(255, 255, 255, 0.85);
        }

        .footer-text {
            font-size: 14px;
            color: #aaa;
            margin-top: 2rem;
            text-align: center;
        }

        @media (max-width: 768px) {
            .navbar {
                padding: 12px 15px !important;
            }

            .navbar .container {
                flex-direction: row;
                align-items: center;
                gap: 12px;
            }

            .navbar-brand img {
                height: 28px;
            }

            .main-container {
                padding-top: 120px;
            }

            .info-item {
                flex-direction: column;
                align-items: flex-start;
                gap: 8px;
                padding: 12px 0;
            }

            .info-label {
                margin-right: 0;
            }

            .info-value {
                text-align: left;
                max-width: 100%;
                width: 100%;
                background: rgba(0, 0, 0, 0.25);
                padding: 10px;
                border-radius: 6px;
                font-size: 13px;
                white-space: nowrap;
                overflow-x: auto;
            }
            
            .actions {
                flex-direction: column;
            }
            
            .button {
                width: 100%;
            }
        }

        .modal-overlay {
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: rgba(0, 0, 0, 0.6);
            backdrop-filter: blur(4px);
            -webkit-backdrop-filter: blur(4px);
            z-index: 2000;
            display: none;
            justify-content: center;
            align-items: center;
            opacity: 0;
            transition: opacity 0.3s ease;
        }

        .modal-overlay.active {
            display: flex;
            opacity: 1;
        }

        .modal-content {
            background: #242424;
            border: 1px solid rgba(84, 84, 88, 0.4);
            border-radius: 16px;
            width: 90%;
            max-width: 500px;
            padding: 32px;
            box-shadow: 0 20px 40px rgba(0, 0, 0, 0.4);
            transform: scale(0.95);
            transition: transform 0.3s cubic-bezier(0.175, 0.885, 0.32, 1.275);
            text-align: left;
        }

        .modal-overlay.active .modal-content {
            transform: scale(1);
        }

        .modal-title {
            font-size: 20px;
            font-weight: 700;
            color: #fff;
            margin-bottom: 16px;
            margin-top: 0;
        }

        .modal-body {
            font-size: 14px;
            line-height: 1.6;
            color: rgba(235, 235, 245, 0.8);
            margin-bottom: 32px;
        }

        .modal-close {
            width: 100%;
        }

        @media (prefers-reduced-motion: reduce) {
            * {
                animation: none !important;
                transition: none !important;
            }
        }
    </style>
</head>
<body>
    <nav class="navbar">
        <div class="container">
            <a class="navbar-brand" href="/">
                <img data-cfasync="false" loading="eager" decoding="async" src="https://cdn.krfoss.org/web/ROKFOSS.png" alt="ROKFOSS">
            </a>
        </div>
    </nav>

    <main id="main" class="main-container" tabindex="-1">
        <div class="content-wrapper">
            <h1>PASTEBOX</h1>
            <p class="subtitle">curl 기반 텍스트/로그 공유 서비스</p>
            <p class="description">curl을 사용하여 텍스트나 로그를 업로드하면 5자리의 무작위 URL이 생성됩니다. 생성된 임시 링크는 30일 후에 자동으로 삭제됩니다.</p>

            <div class="info-section">
                <div class="info-item">
                    <span class="info-label">텍스트 업로드</span>
                    <span class="info-value">echo "hello" | curl -X POST --data-binary @- {{ .BaseURL }}/</span>
                </div>
                <div class="info-item">
                    <span class="info-label">명령어 출력 업로드</span>
                    <span class="info-value">ifconfig | curl -X POST --data-binary @- {{ .BaseURL }}/</span>
                </div>
                <div class="info-item">
                    <span class="info-label">텍스트 파일 업로드</span>
                    <span class="info-value">curl -F "file=@test.txt" {{ .BaseURL }}/</span>
                </div>
                <div class="info-item">
                    <span class="info-label">비밀번호 보호 링크</span>
                    <span class="info-value">curl -H "usepassword: true" -F "file=@secret.txt" {{ .BaseURL }}/</span>
                </div>
                <div class="info-item">
                    <span class="info-label">1회 열람 후 파기</span>
                    <span class="info-value">ifconfig | curl -X POST --data-binary @- {{ .BaseURL }}/temp</span>
                </div>

                <div class="info-item">
                    <span class="info-label">만료 정보</span>
                    <span class="info-value">주소: {{ .BaseURL }}/AbC12
만료일: 2026-06-24T05:10:26Z
삭제링크: {{ .BaseURL }}/AbC12?delete=DELETE_TOKEN</span>
                </div>
                <div class="info-item">
                    <span class="info-label">수동 삭제</span>
                    <span class="info-value">curl "{{ .BaseURL }}/AbC12?delete=DELETE_TOKEN"</span>
                </div>
            </div>

            <div class="actions">
                <a class="button button-primary" href="/">홈</a>
                <button class="button" onclick="copyExample(this)">curl 예시 복사</button>
            </div>
            
            <div class="footer-text">
                <a href="#" onclick="openToSModal(event)" style="color: inherit; text-decoration: underline; cursor: pointer;">서비스 이용 약관</a>
            </div>
        </div>
    </main>

    <!-- ToS Modal -->
    <div class="modal-overlay" id="tosModal" onclick="if(event.target===this) closeToSModal()">
        <div class="modal-content">
            <h2 class="modal-title">서비스 이용 약관</h2>
            <div class="modal-body">
                본 서비스는 임시 텍스트, 코드 및 로그 공유를 목적으로 제공됩니다.<br><br>
                개인 클라우드 스토리지 용도 등 본래 목적에 어긋나는 악의적인 대용량 데이터 업로드 및 서비스 남용 시, 사전 통보 없이 데이터가 즉시 삭제되거나 서비스 이용이 영구적으로 차단될 수 있습니다.
            </div>
            <button class="button modal-close" onclick="closeToSModal()">확인</button>
        </div>
    </div>

    <script>
        function openToSModal(e) {
            e.preventDefault();
            document.getElementById('tosModal').classList.add('active');
        }

        function closeToSModal() {
            document.getElementById('tosModal').classList.remove('active');
        }

        function copyExample(btn) {
            const text = 'curl -F "file=@test.txt" {{ .BaseURL }}/';
            navigator.clipboard.writeText(text).then(() => {
                const originalText = btn.innerText;
                btn.innerText = "복사 완료";
                setTimeout(() => {
                    btn.innerText = originalText;
                }, 1500);
            }).catch(err => {
                const textarea = document.createElement("textarea");
                textarea.value = text;
                textarea.style.position = "fixed";
                textarea.style.left = "-9999px";
                document.body.appendChild(textarea);
                textarea.select();
                document.execCommand("copy");
                document.body.removeChild(textarea);
                
                const originalText = btn.innerText;
                btn.innerText = "복사 완료";
                setTimeout(() => {
                    btn.innerText = originalText;
                }, 1500);
            });
        }
    </script>
</body>
</html>`

func generateRandomToken(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		result[i] = alphabet[n.Int64()]
	}
	return string(result), nil
}

func ensureAdminToken(path string, cfg *config) error {
	trimmedToken := strings.TrimSpace(cfg.AdminToken)
	if trimmedToken != "" {
		cfg.AdminToken = trimmedToken
		return nil
	}

	token, err := generateRandomToken(256)
	if err != nil {
		return err
	}
	cfg.AdminToken = token

	log.Printf("================================================================================\n")
	log.Printf("새로운 ADMIN_TOKEN이 자동으로 생성되었습니다. 로그인 시 다음 토큰을 사용하십시오:\n%s\n", token)
	log.Printf("================================================================================\n")

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			content := fmt.Sprintf("STORAGE_MODE=%s\nLISTEN_ADDR=%s\nDATA_DIR=%s\nEXPIRE_DAYS=%d\nDB_DSN=%s\nDB_COMPRESSION_ALGORITHM=%s\nADMIN_TOKEN=%s\n",
				cfg.StorageMode, cfg.ListenAddr, cfg.DataDir, cfg.ExpireDays, cfg.DBDSN, cfg.DBCompress, token)
			return os.WriteFile(path, []byte(content), 0644)
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "ADMIN_TOKEN") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) > 0 {
				lines[i] = "ADMIN_TOKEN=" + token
				found = true
				break
			}
		}
	}

	if !found {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "ADMIN_TOKEN="+token)
	}

	newContent := strings.Join(lines, "\n")
	return os.WriteFile(path, []byte(newContent), 0644)
}

func (a *app) getStorageModeString() string {
	switch a.store.(type) {
	case *pastebox.DBStore:
		return "db"
	default:
		return "local"
	}
}

func (a *app) adminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_token")
	if err != nil || cookie.Value != a.adminToken || a.adminToken == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = adminLoginHTML.Execute(w, map[string]any{
			"Error": "",
		})
		return
	}

	pastes, err := a.store.List()
	if err != nil {
		http.Error(w, "데이터 조회 실패", http.StatusInternalServerError)
		return
	}

	// 기존 영구 저장 데이터가 있을 경우 화면 표시용 예상 만료일을 주입합니다.
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
	_ = adminDashboardHTML.Execute(w, map[string]any{
		"Pastes":      pastes,
		"StorageMode": a.getStorageModeString(),
	})
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
		_ = adminLoginHTML.Execute(w, map[string]any{
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

var adminLoginHTML = template.Must(template.New("admin_login").Parse(`<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>관리자 로그인 - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#0d0d0e] text-zinc-100 flex items-center justify-center p-4">
  <div class="w-full max-w-md bg-zinc-900/60 backdrop-blur-xl border border-zinc-800/80 rounded-2xl p-8 shadow-2xl transition-all duration-300 hover:border-zinc-700/80">
    <div class="mb-6 text-center">
      <div class="inline-flex rounded-full bg-amber-500/10 border border-amber-500/20 px-3 py-1 text-xs font-medium text-amber-400 mb-2">
        관리자 콘솔
      </div>
      <h1 class="text-2xl font-bold tracking-tight text-white">Pastebox Admin</h1>
      <p class="mt-2 text-sm text-zinc-400">대시보드 진입을 위해 256자 토큰을 입력해 주세요.</p>
    </div>

    {{ if .Error }}
    <div class="mb-4 rounded-xl bg-red-500/10 border border-red-500/20 p-3 text-xs text-red-400 text-center">
      {{ .Error }}
    </div>
    {{ end }}

    <form action="/ra/login" method="POST" class="space-y-4">
      <div>
        <label for="token" class="block text-xs font-semibold text-zinc-400 uppercase tracking-wider mb-2">Access Token</label>
        <textarea
          id="token"
          name="token"
          rows="4"
          required
          placeholder="256자 토큰을 여기에 붙여넣으세요..."
          class="w-full rounded-xl border border-zinc-800 bg-zinc-950/80 p-3 text-xs font-mono text-zinc-300 placeholder-zinc-600 focus:border-zinc-700 focus:ring-1 focus:ring-zinc-700 focus:outline-none transition"
        ></textarea>
      </div>
      <button
        type="submit"
        class="w-full rounded-xl bg-zinc-100 py-3 font-semibold text-zinc-950 hover:bg-white active:scale-[0.98] transition duration-200"
      >
        로그인
      </button>
    </form>
  </div>
</body>
</html>`))

var adminDashboardHTML = template.Must(template.New("admin_dashboard").Parse(`<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>관리 대시보드 - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <script>
    tailwind.config = {
      darkMode: 'class'
    }
  </script>
</head>
<body class="dark min-h-screen bg-[#0B0B0C] text-zinc-100 font-sans">
  <!-- 상단 네비게이션 -->
  <header class="border-b border-zinc-900 bg-zinc-950/60 backdrop-blur-md sticky top-0 z-50">
    <div class="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8 h-16 flex items-center justify-between">
      <div class="flex items-center gap-3">
        <span class="text-xl font-bold tracking-tight text-white">Pastebox <span class="text-zinc-600 font-normal">/ Admin</span></span>
        <span class="inline-flex items-center rounded-full bg-emerald-500/10 border border-emerald-500/20 px-2 py-0.5 text-xs font-medium text-emerald-400">
          온라인
        </span>
      </div>
      <div class="flex items-center gap-4">
        <a href="/" target="_blank" class="text-xs text-zinc-400 hover:text-white transition">사용자 홈 ↗</a>
        <a href="/ra/logout" class="rounded-xl border border-zinc-800 hover:border-zinc-700 bg-zinc-900 px-3.5 py-1.5 text-xs font-semibold text-zinc-300 hover:bg-zinc-800 transition">
          로그아웃
        </a>
      </div>
    </div>
  </header>

  <main class="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8 py-8">
    <!-- 정보 카드 섹션 -->
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 mb-8">
      <div class="rounded-2xl border border-zinc-900 bg-zinc-950/40 p-5 backdrop-blur-sm">
        <p class="text-xs font-medium text-zinc-500 uppercase tracking-wider">스토리지 상태</p>
        <p class="mt-2 text-2xl font-bold tracking-tight text-white uppercase">{{ .StorageMode }} Mode</p>
      </div>
      <div class="rounded-2xl border border-zinc-900 bg-zinc-950/40 p-5 backdrop-blur-sm">
        <p class="text-xs font-medium text-zinc-500 uppercase tracking-wider">전체 Paste 개수</p>
        <p class="mt-2 text-2xl font-bold tracking-tight text-white">{{ len .Pastes }}개</p>
      </div>
    </div>

    <!-- 액션 제어 패널 -->
    <div class="mb-4 flex items-center justify-between gap-4">
      <div class="flex items-center gap-2">
        <button
          onclick="deleteSelected()"
          class="rounded-xl bg-red-500/10 border border-red-500/20 hover:border-red-500/40 px-3.5 py-2 text-xs font-semibold text-red-400 hover:bg-red-500/20 transition disabled:opacity-50 disabled:pointer-events-none"
          id="btnDeleteSelected"
          disabled
        >
          선택 삭제
        </button>
        <button
          onclick="confirmDeleteAll()"
          class="rounded-xl bg-zinc-900 border border-zinc-800 hover:border-red-900/60 px-3.5 py-2 text-xs font-semibold text-zinc-400 hover:text-red-400 hover:bg-red-950/20 transition"
        >
          전체 삭제
        </button>
      </div>
      <div class="text-xs text-zinc-500">
        * 모든 저장 데이터는 즉시 파기 가능합니다.
      </div>
    </div>

    <!-- 데이터 테이블 -->
    <div class="overflow-hidden rounded-2xl border border-zinc-900 bg-zinc-950/20 backdrop-blur-sm">
      <div class="overflow-x-auto">
        <table class="min-w-full divide-y divide-zinc-900 text-left text-sm">
          <thead class="bg-zinc-950/60 text-xs font-semibold text-zinc-400 uppercase tracking-wider">
            <tr>
              <th scope="col" class="w-12 px-6 py-4">
                <input
                  type="checkbox"
                  id="selectAll"
                  onclick="toggleAll(this)"
                  class="h-4 w-4 rounded border-zinc-800 bg-zinc-900 text-zinc-600 focus:ring-0 focus:ring-offset-0 focus:outline-none"
                >
              </th>
              <th scope="col" class="px-6 py-4">ID / 링크</th>
              <th scope="col" class="px-6 py-4">콘텐츠 타입</th>
              <th scope="col" class="px-6 py-4">파일 크기</th>
              <th scope="col" class="px-6 py-4">생성일</th>
              <th scope="col" class="px-6 py-4">만료일</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-900/60 bg-transparent text-zinc-300">
            {{ range .Pastes }}
            <tr class="hover:bg-zinc-900/20 transition-colors">
              <td class="px-6 py-4">
                <input
                  type="checkbox"
                  name="ids"
                  value="{{ .ID }}"
                  onclick="updateSelection()"
                  class="h-4 w-4 rounded border-zinc-800 bg-zinc-900 text-zinc-600 focus:ring-0 focus:ring-offset-0 focus:outline-none"
                >
              </td>
              <td class="px-6 py-4 font-mono font-medium text-zinc-100">
                <a href="/{{ .ID }}" target="_blank" class="hover:underline text-zinc-200 hover:text-white transition">
                  {{ .ID }} ↗
                </a>
              </td>
              <td class="px-6 py-4 text-xs text-zinc-400">{{ .ContentType }}</td>
              <td class="px-6 py-4 text-xs">{{ .Size }} Bytes</td>
              <td class="px-6 py-4 text-xs text-zinc-500">{{ .CreatedAt.Local.Format "2006-01-02 15:04:05" }}</td>
              <td class="px-6 py-4 text-xs text-zinc-500">
                {{ if .ExpiresAt.IsZero }}
                -
                {{ else }}
                {{ .ExpiresAt.Local.Format "2006-01-02 15:04:05" }}
                {{ end }}
              </td>
            </tr>
            {{ else }}
            <tr>
              <td colspan="7" class="px-6 py-12 text-center text-zinc-500 text-xs">
                업로드된 Paste가 없습니다.
              </td>
            </tr>
            {{ end }}
          </tbody>
        </table>
      </div>
    </div>
  </main>

  <!-- 확인 모달 -->
  <div id="confirmModal" class="hidden fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/60 backdrop-blur-sm">
    <div class="w-full max-w-sm rounded-2xl border border-zinc-800 bg-zinc-900 p-6 shadow-2xl animate-in fade-in zoom-in-95 duration-150">
      <h3 class="text-base font-semibold text-white" id="modalTitle">정말로 삭제하시겠습니까?</h3>
      <p class="mt-2 text-xs text-zinc-400" id="modalDescription">삭제된 데이터는 절대 복구할 수 없습니다.</p>
      <div class="mt-6 flex justify-end gap-3">
        <button
          onclick="closeModal()"
          class="rounded-xl bg-zinc-800 border border-zinc-700 px-4 py-2 text-xs font-semibold text-zinc-300 hover:bg-zinc-700 transition"
        >
          취소
        </button>
        <button
          id="modalConfirmBtn"
          class="rounded-xl bg-red-600 hover:bg-red-500 px-4 py-2 text-xs font-semibold text-white transition active:scale-95"
        >
          삭제하기
        </button>
      </div>
    </div>
  </div>

  <form id="actionForm" method="POST" class="hidden">
    <input type="hidden" id="actionFormIds" name="ids">
  </form>

  <script>
    let activeAction = null;

    function toggleAll(master) {
      const checkboxes = document.getElementsByName('ids');
      for (let cb of checkboxes) {
        cb.checked = master.checked;
      }
      updateSelection();
    }

    function updateSelection() {
      const checkboxes = document.getElementsByName('ids');
      let count = 0;
      for (let cb of checkboxes) {
        if (cb.checked) count++;
      }
      const btn = document.getElementById('btnDeleteSelected');
      btn.disabled = count === 0;
    }

    function getSelectedIds() {
      const checkboxes = document.getElementsByName('ids');
      const ids = [];
      for (let cb of checkboxes) {
        if (cb.checked) ids.push(cb.value);
      }
      return ids;
    }

    function deleteSelected() {
      const ids = getSelectedIds();
      if (ids.length === 0) return;

      showModal(
        '선택한 ' + ids.length + '개의 항목을 삭제합니까?',
        '삭제하면 서버에서 데이터 파일 및 DB 기록이 완전히 지워집니다.',
        function() {
          const form = document.getElementById('actionForm');
          form.action = '/ra/delete';
          document.getElementById('actionFormIds').value = ids.join(',');
          form.submit();
        }
      );
    }

    function confirmDeleteAll() {
      showModal(
        '서버의 모든 Paste를 삭제합니까?',
        '경고: 모든 업로드 데이터가 완전히 영구 파기됩니다. 이 작업은 되돌릴 수 없습니다.',
        function() {
          const form = document.getElementById('actionForm');
          form.action = '/ra/delete-all';
          form.submit();
        }
      );
    }

    function showModal(title, desc, confirmCallback) {
      document.getElementById('modalTitle').innerText = title;
      document.getElementById('modalDescription').innerText = desc;
      const confirmBtn = document.getElementById('modalConfirmBtn');
      
      // 기존 이벤트 제거 및 새 이벤트 바인딩
      const newConfirmBtn = confirmBtn.cloneNode(true);
      confirmBtn.parentNode.replaceChild(newConfirmBtn, confirmBtn);
      
      newConfirmBtn.addEventListener('click', function() {
        closeModal();
        confirmCallback();
      });

      document.getElementById('confirmModal').classList.remove('hidden');
    }

    function closeModal() {
      document.getElementById('confirmModal').classList.add('hidden');
    }
  </script>
</body>
</html>`))
