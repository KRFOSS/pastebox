package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
)

type config struct {
	StorageMode     string
	ListenAddr      string
	DataDir         string
	ExpireDays      int
	DBDSN           string
	DBCompress      string
	AdminToken      string
	MaxUploadSizeMB int64
}

func loadConfig(path string) (*config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &config{
		StorageMode:     "local",
		ListenAddr:      ":8080",
		DataDir:         "./data",
		ExpireDays:      30,
		DBCompress:      "zstd",
		MaxUploadSizeMB: 10,
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
		case "MAX_UPLOAD_SIZE_MB":
			if sz, err := strconv.ParseInt(val, 10, 64); err == nil {
				cfg.MaxUploadSizeMB = sz
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cfg, nil
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
