package main

import (
	"bufio"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"

	pastebox "pastebox/internal"
)

type config struct {
	StorageMode         string
	ListenAddr          string
	DataDir             string
	ExpireDays          int
	DBDSN               string
	DBCompress          string
	AdminToken          string
	MaxUploadSizeMB     int64
	RateLimitPerSec     float64
	RateBurst           float64
	HomeBackgroundImage string
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
		RateLimitPerSec: 2,
		RateBurst:       10,
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
		case "RATE_LIMIT_PER_SEC":
			if v, err := strconv.ParseFloat(val, 64); err == nil && v > 0 {
				cfg.RateLimitPerSec = v
			}
		case "RATE_LIMIT_BURST":
			if v, err := strconv.ParseFloat(val, 64); err == nil && v > 0 {
				cfg.RateBurst = v
			}
		case "HOME_BACKGROUND_IMAGE_URL":
			cfg.HomeBackgroundImage = val
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func persistConfigValues(path string, values map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	normalized := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.ToUpper(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		normalized[key] = strings.NewReplacer("\r", "", "\n", "").Replace(value)
	}

	lines := strings.Split(string(data), "\n")
	written := make(map[string]bool, len(normalized))
	result := make([]string, 0, len(lines)+len(normalized))
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			result = append(result, line)
			continue
		}

		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		value, shouldUpdate := normalized[key]
		if !shouldUpdate {
			result = append(result, line)
			continue
		}
		if written[key] {
			continue
		}
		result = append(result, key+"="+value)
		written[key] = true
	}

	for key, value := range normalized {
		if !written[key] {
			result = append(result, key+"="+value)
		}
	}

	content := strings.TrimRight(strings.Join(result, "\n"), "\n") + "\n"
	return writeConfigFile(path, []byte(content))
}

func writeConfigFile(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func (a *app) configFilePath() string {
	return a.configPath
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

func getenvFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	f, err := strconv.ParseFloat(value, 64)
	if err != nil || f <= 0 {
		return fallback
	}

	return f
}

func ensureAdminToken(path string, cfg *config) error {
	trimmedToken := strings.TrimSpace(cfg.AdminToken)
	if trimmedToken != "" {
		cfg.AdminToken = trimmedToken
		return nil
	}

	token, err := pastebox.RandomString(pastebox.AlphanumericAlphabet, 256)
	if err != nil {
		return err
	}
	cfg.AdminToken = token

	log.Printf("================================================================================\n")
	log.Printf("새로운 ADMIN_TOKEN이 자동으로 생성되었습니다. 로그인 시 다음 토큰을 사용하십시오:\n%s\n", token)
	log.Printf("================================================================================\n")

	if writeErr := persistAdminToken(path, cfg, token); writeErr != nil {
		log.Printf("경고: ADMIN_TOKEN을 파일에 기록할 수 없습니다 (읽기 전용 환경?). 토큰은 현재 세션에서만 유효합니다: %v", writeErr)
	}

	return nil
}

func persistAdminToken(path string, cfg *config, token string) error {
	return persistConfigValues(path, map[string]string{"ADMIN_TOKEN": token})
}
