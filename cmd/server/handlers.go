package main

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	pastebox "pastebox/internal"
)

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

type app struct {
	store         pastebox.Storage
	index         *template.Template
	pasteView     *template.Template
	password      *template.Template
	adminLogin    *template.Template
	adminDashboard *template.Template
	adminToken    string
	expireDays    int
	maxUploadSize int64
	mu            sync.RWMutex
}

func (a *app) getMaxUploadSize() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.maxUploadSize
}

func (a *app) getStorageModeString() string {
	switch a.store.(type) {
	case *pastebox.DBStore:
		return "db"
	default:
		return "local"
	}
}

func (a *app) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/robots.txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/temp" || r.URL.Path == "/pw" {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path == "/temp" || r.URL.Path == "/pw" {
				http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
				return
			}
			a.indexHandler(w, r)
		case http.MethodPost, http.MethodPut:
			if r.URL.Path == "/temp" {
				r.Header.Set("data-policy", "once")
			}
			if r.URL.Path == "/pw" {
				r.Header.Set("usepassword", "true")
			}
			a.uploadHandler(w, r)
		default:
			http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		}
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
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
	maxUploadSize := a.getMaxUploadSize()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	var reader io.Reader
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			http.Error(w, fmt.Sprintf("업로드 용량(%.1fMB) 초과 또는 유효하지 않은 요청입니다.", float64(maxUploadSize)/(1024*1024)), http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "업로드 파일 필드가 누락되었습니다.", http.StatusBadRequest)
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
		policy = r.URL.Query().Get("data-policy")
	}
	if policy == "" && r.PostForm != nil {
		policy = r.PostForm.Get("data-policy")
	}
	once := strings.EqualFold(strings.TrimSpace(policy), "once")

	meta, password, deleteToken, err := a.store.Create(reader, contentType, usePassword, once)
	if err != nil {
		log.Printf("업로드 실패: %v", err)

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
			log.Printf("삭제 거부됨: ID=%s, 원격 IP=%s", id, r.RemoteAddr)
			http.Error(w, "삭제 토큰이 누락되었거나 유효하지 않습니다.", http.StatusUnauthorized)
			return
		}

		http.NotFound(w, r)
		return
	}

	log.Printf("삭제 완료됨: ID=%s, 원격 IP=%s", id, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "삭제되었습니다.")
}

func (a *app) viewHandler(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method == http.MethodPost {
		password := r.FormValue("password")
		if password == "" {
			http.Error(w, "비밀번호가 입력되지 않았습니다.", http.StatusBadRequest)
			return
		}
		
		_, err := a.store.Stat(id, password)
		if err != nil {
			if errors.Is(err, pastebox.ErrInvalidPassword) {
				_ = a.password.Execute(w, map[string]any{
					"Action": "?",
					"Error":  "비밀번호가 틀렸습니다.",
				})
				return
			}
			http.NotFound(w, r)
			return
		}
		
		cookieName := fmt.Sprintf("paste_auth_%s", id)
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    password,
			Path:     "/" + id,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   3600,
		})
		
		http.Redirect(w, r, "/"+id, http.StatusFound)
		return
	}

	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("paste-password")
	}
	if password == "" {
		if cookie, err := r.Cookie(fmt.Sprintf("paste_auth_%s", id)); err == nil {
			password = cookie.Value
		}
	}

	raw := r.URL.Query().Get("raw") == "1"
	browser := isBrowserRequest(r)

	if !raw && browser {
		meta, err := a.store.Stat(id, password)
		if err != nil {
			if errors.Is(err, pastebox.ErrInvalidPassword) {
				w.WriteHeader(http.StatusUnauthorized)
				_ = a.password.Execute(w, map[string]any{
					"Action": "?",
					"Error":  "",
				})
				return
			}
			http.NotFound(w, r)
			return
		}

		if meta.PasswordHash != "" || strings.EqualFold(meta.DataPolicy, "once") {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_ = a.pasteView.Execute(w, map[string]any{
			"ID":      meta.ID,
			"Content": "",
		})
		return
	}

	entry, err := a.store.Open(id, password)
	if err != nil {
		if errors.Is(err, pastebox.ErrInvalidPassword) {
			w.WriteHeader(http.StatusUnauthorized)
			if browser {
				_ = a.password.Execute(w, map[string]any{
					"Action": "?",
					"Error":  "",
				})
			} else {
				fmt.Fprintln(w, "비밀번호가 필요하거나 유효하지 않습니다. -H 'paste-password: ...' 헤더를 사용하세요.")
			}
			return
		}
		http.NotFound(w, r)
		return
	}
	defer entry.File.Close()

	if entry.Meta.PasswordHash != "" || strings.EqualFold(entry.Meta.DataPolicy, "once") {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
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
