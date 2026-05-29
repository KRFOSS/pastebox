package internal

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrInvalidPassword    = errors.New("invalid password")
	ErrInvalidDeleteToken = errors.New("invalid delete token")
)

type Storage interface {
	Create(r io.Reader, contentType string, usePassword bool, once bool) (meta Metadata, password string, deleteToken string, err error)
	Open(id string, password string) (entry *Entry, err error)
	Stat(id string, password string) (meta Metadata, err error)
	Delete(id string, token string) error
	CleanupExpired() error
	Close() error
	List() ([]Metadata, error)
	ForceDelete(id string) error
	DeleteAll() error
}

type Metadata struct {
	ID              string    `json:"id"`
	PasswordHash    string    `json:"password_hash,omitempty"`
	DeleteTokenHash string    `json:"delete_token_hash,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	DataPolicy      string    `json:"data_policy,omitempty"`
	Size            int64     `json:"size"`
	ContentType     string    `json:"content_type"`
}

type Entry struct {
	Meta Metadata
	File io.ReadSeekCloser
}

// LocalStore는 로컬 파일 시스템을 사용해 Storage를 구현합니다.
type LocalStore struct {
	DataDir string
	TTL     time.Duration
	lockMgr *lockManager
}

func NewLocalStore(dataDir string, ttl time.Duration) (*LocalStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	return &LocalStore{
		DataDir: dataDir,
		TTL:     ttl,
		lockMgr: newLockManager(),
	}, nil
}

func (s *LocalStore) Create(r io.Reader, contentType string, usePassword bool, once bool) (Metadata, string, string, error) {
	id, path, err := s.reservePath()
	if err != nil {
		return Metadata{}, "", "", err
	}

	l, release := s.lockMgr.acquire(id)
	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
		release()
	}()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Metadata{}, "", "", err
	}

	size, copyErr := io.Copy(file, r)
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(path)
		return Metadata{}, "", "", copyErr
	}

	if closeErr != nil {
		_ = os.Remove(path)
		return Metadata{}, "", "", closeErr
	}

	var password string
	var passwordHash string

	if usePassword {
		password, err = generatePassword(8)
		if err != nil {
			_ = os.Remove(path)
			return Metadata{}, "", "", err
		}
		passwordHash = hashSecret(password)
	}

	deleteToken, err := randomString(tokenAlphabet, 32)
	if err != nil {
		_ = os.Remove(path)
		return Metadata{}, "", "", err
	}

	now := time.Now().UTC()

	dataPolicy := "temporary"
	if once {
		dataPolicy = "once"
	}
	expiresAt := now.Add(s.TTL)

	meta := Metadata{
		ID:              id,
		PasswordHash:    passwordHash,
		DeleteTokenHash: hashSecret(deleteToken),
		CreatedAt:       now,
		ExpiresAt:       expiresAt,
		DataPolicy:      dataPolicy,
		Size:            size,
		ContentType:     contentType,
	}

	if err := s.writeMetadata(meta); err != nil {
		_ = os.Remove(path)
		return Metadata{}, "", "", err
	}

	return meta, password, deleteToken, nil
}

func (s *LocalStore) Open(id string, password string) (*Entry, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}

	path := s.path(id)

	l, release := s.lockMgr.acquire(id)
	l.mu.RLock()

	var unlocked bool
	defer func() {
		if !unlocked {
			l.mu.RUnlock()
			release()
		}
	}()

	meta, err := s.readMetadata(id)
	if err != nil {
		return nil, ErrNotFound
	}

	if isExpired(meta, time.Now().UTC(), s.TTL) {
		l.mu.RUnlock()
		l.mu.Lock()
		metaDouble, errDouble := s.readMetadata(id)
		if errDouble == nil && isExpired(metaDouble, time.Now().UTC(), s.TTL) {
			_ = os.Remove(path)
			_ = os.Remove(metaPath(path))
		}
		l.mu.Unlock()
		unlocked = true
		release()
		return nil, ErrNotFound
	}

	if meta.PasswordHash != "" {
		if strings.TrimSpace(password) == "" || !checkSecret(password, meta.PasswordHash) {
			return nil, ErrInvalidPassword
		}
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, ErrNotFound
	}

	if strings.EqualFold(meta.DataPolicy, "once") {
		// Burn after reading
		l.mu.RUnlock()
		l.mu.Lock()
		_ = os.Remove(path)
		_ = os.Remove(metaPath(path))
		l.mu.Unlock()
		unlocked = true
		release()
	}

	return &Entry{
		Meta: meta,
		File: file,
	}, nil
}

func (s *LocalStore) Stat(id string, password string) (Metadata, error) {
	if !validID(id) {
		return Metadata{}, ErrNotFound
	}

	l, release := s.lockMgr.acquire(id)
	l.mu.RLock()
	defer release()
	defer l.mu.RUnlock()

	meta, err := s.readMetadata(id)
	if err != nil {
		return Metadata{}, ErrNotFound
	}

	if isExpired(meta, time.Now().UTC(), s.TTL) {
		return Metadata{}, ErrNotFound
	}

	if meta.PasswordHash != "" {
		if strings.TrimSpace(password) == "" || !checkSecret(password, meta.PasswordHash) {
			return Metadata{}, ErrInvalidPassword
		}
	}

	return meta, nil
}

func (s *LocalStore) Delete(id string, token string) error {
	if !validID(id) {
		return ErrNotFound
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return ErrInvalidDeleteToken
	}

	l, release := s.lockMgr.acquire(id)
	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
		release()
	}()

	path := s.path(id)

	meta, err := s.readMetadata(id)
	if err != nil {
		return ErrNotFound
	}

	if meta.DeleteTokenHash == "" || !checkSecret(token, meta.DeleteTokenHash) {
		return ErrInvalidDeleteToken
	}

	fileErr := os.Remove(path)
	metaErr := os.Remove(metaPath(path))

	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return fileErr
	}

	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		return metaErr
	}

	return nil
}

func (s *LocalStore) CleanupExpired() error {
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		if strings.HasSuffix(name, ".json") {
			continue
		}

		if !validID(name) {
			continue
		}

		l, release := s.lockMgr.acquire(name)
		l.mu.Lock()

		meta, err := s.readMetadata(name)
		if err == nil && isExpired(meta, now, s.TTL) {
			path := s.path(name)
			_ = os.Remove(path)
			_ = os.Remove(metaPath(path))
		}

		l.mu.Unlock()
		release()
	}

	return nil
}

func (s *LocalStore) Close() error {
	return nil
}

func (s *LocalStore) List() ([]Metadata, error) {
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return nil, err
	}

	var list []Metadata
	now := time.Now().UTC()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasSuffix(name, ".json") {
			id := strings.TrimSuffix(name, ".json")
			if !validID(id) {
				continue
			}

			l, release := s.lockMgr.acquire(id)
			l.mu.RLock()
			meta, err := s.readMetadata(id)
			l.mu.RUnlock()
			release()

			if err == nil && !isExpired(meta, now, s.TTL) {
				list = append(list, meta)
			}
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})

	return list, nil
}

func (s *LocalStore) ForceDelete(id string) error {
	if !validID(id) {
		return ErrNotFound
	}

	l, release := s.lockMgr.acquire(id)
	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
		release()
	}()

	path := s.path(id)
	fileErr := os.Remove(path)
	metaErr := os.Remove(metaPath(path))

	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return fileErr
	}
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		return metaErr
	}

	return nil
}

func (s *LocalStore) DeleteAll() error {
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		id := name
		if strings.HasSuffix(name, ".json") {
			id = strings.TrimSuffix(name, ".json")
		}

		if !validID(id) {
			continue
		}

		l, release := s.lockMgr.acquire(id)
		l.mu.Lock()
		_ = os.Remove(s.path(id))
		_ = os.Remove(metaPath(s.path(id)))
		l.mu.Unlock()
		release()
	}

	return nil
}

func (s *LocalStore) reservePath() (string, string, error) {
	for i := 0; i < 100; i++ {
		id, err := randomString(idAlphabet, 5)
		if err != nil {
			return "", "", err
		}

		path := s.path(id)

		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return id, path, nil
		}
	}

	return "", "", errors.New("failed to reserve random id")
}

func (s *LocalStore) path(id string) string {
	return filepath.Join(s.DataDir, id)
}

func (s *LocalStore) writeMetadata(meta Metadata) error {
	path := s.path(meta.ID)
	metaFile := metaPath(path)

	tmp := metaFile + ".tmp"

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, metaFile)
}

func (s *LocalStore) readMetadata(id string) (Metadata, error) {
	var meta Metadata

	data, err := os.ReadFile(metaPath(s.path(id)))
	if err != nil {
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}

	return meta, nil
}

// DBStore는 MariaDB(MySQL) 및 압축을 사용해 Storage를 구현합니다.
type DBStore struct {
	db           *sql.DB
	TTL          time.Duration
	compressAlgo string
}

func NewDBStore(dsn string, ttl time.Duration, compressAlgo string) (*DBStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	db.SetConnMaxLifetime(time.Minute * 3)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	store := &DBStore{
		db:           db,
		TTL:          ttl,
		compressAlgo: strings.ToLower(strings.TrimSpace(compressAlgo)),
	}

	if err := store.autoMigrate(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *DBStore) autoMigrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS pastes (
		id VARCHAR(5) PRIMARY KEY,
		password_hash VARCHAR(64) NULL,
		delete_token_hash VARCHAR(64) NOT NULL,
		created_at DATETIME NOT NULL,
		expires_at DATETIME NULL,
		data_policy VARCHAR(16) NOT NULL,
		size BIGINT NOT NULL,
		content_type VARCHAR(128) NOT NULL,
		content LONGBLOB NOT NULL,
		compressed_algo VARCHAR(16) NOT NULL
	);`
	if _, err := s.db.Exec(query); err != nil {
		return err
	}

	indexQuery := `CREATE INDEX IF NOT EXISTS idx_pastes_expires_at ON pastes(expires_at);`
	_, _ = s.db.Exec(indexQuery)

	return nil
}

func (s *DBStore) Create(r io.Reader, contentType string, usePassword bool, once bool) (Metadata, string, string, error) {
	var id string
	var err error
	for i := 0; i < 100; i++ {
		id, err = randomString(idAlphabet, 5)
		if err != nil {
			return Metadata{}, "", "", err
		}

		var exists int
		err := s.db.QueryRow("SELECT COUNT(*) FROM pastes WHERE id = ?", id).Scan(&exists)
		if err != nil {
			return Metadata{}, "", "", err
		}
		if exists == 0 {
			break
		}
		if i == 99 {
			return Metadata{}, "", "", errors.New("failed to reserve random id in DB")
		}
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return Metadata{}, "", "", err
	}
	size := int64(len(data))

	compressedData, err := compressData(data, s.compressAlgo)
	if err != nil {
		return Metadata{}, "", "", err
	}

	var password string
	var passwordHash string
	if usePassword {
		password, err = generatePassword(8)
		if err != nil {
			return Metadata{}, "", "", err
		}
		passwordHash = hashSecret(password)
	}

	deleteToken, err := randomString(tokenAlphabet, 32)
	if err != nil {
		return Metadata{}, "", "", err
	}

	now := time.Now().UTC()
	dataPolicy := "temporary"
	if once {
		dataPolicy = "once"
	}
	expiresAt := now.Add(s.TTL)

	meta := Metadata{
		ID:              id,
		PasswordHash:    passwordHash,
		DeleteTokenHash: hashSecret(deleteToken),
		CreatedAt:       now,
		ExpiresAt:       expiresAt,
		DataPolicy:      dataPolicy,
		Size:            size,
		ContentType:     contentType,
	}

	var expiresAtNull sql.NullTime
	if !expiresAt.IsZero() {
		expiresAtNull = sql.NullTime{Time: expiresAt, Valid: true}
	}

	var dbPassHash sql.NullString
	if passwordHash != "" {
		dbPassHash = sql.NullString{String: passwordHash, Valid: true}
	}

	insertQuery := `
	INSERT INTO pastes (id, password_hash, delete_token_hash, created_at, expires_at, data_policy, size, content_type, content, compressed_algo)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = s.db.Exec(insertQuery, id, dbPassHash, meta.DeleteTokenHash, now, expiresAtNull, dataPolicy, size, contentType, compressedData, s.compressAlgo)
	if err != nil {
		return Metadata{}, "", "", err
	}

	return meta, password, deleteToken, nil
}

func (s *DBStore) Open(id string, password string) (*Entry, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}

	var meta Metadata
	var dbPassHash sql.NullString
	var expiresAtNull sql.NullTime
	var compressedData []byte
	var algo string

	query := `
	SELECT id, password_hash, delete_token_hash, created_at, expires_at, data_policy, size, content_type, content, compressed_algo
	FROM pastes
	WHERE id = ?`

	err := s.db.QueryRow(query, id).Scan(
		&meta.ID,
		&dbPassHash,
		&meta.DeleteTokenHash,
		&meta.CreatedAt,
		&expiresAtNull,
		&meta.DataPolicy,
		&meta.Size,
		&meta.ContentType,
		&compressedData,
		&algo,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if dbPassHash.Valid {
		meta.PasswordHash = dbPassHash.String
	}
	if expiresAtNull.Valid {
		meta.ExpiresAt = expiresAtNull.Time
	}

	if isExpired(meta, time.Now().UTC(), s.TTL) {
		_, _ = s.db.Exec("DELETE FROM pastes WHERE id = ?", id)
		return nil, ErrNotFound
	}

	if meta.PasswordHash != "" {
		if strings.TrimSpace(password) == "" || !checkSecret(password, meta.PasswordHash) {
			return nil, ErrInvalidPassword
		}
	}

	rc, err := decompressData(compressedData, algo)
	if err != nil {
		return nil, err
	}

	if strings.EqualFold(meta.DataPolicy, "once") {
		_, _ = s.db.Exec("DELETE FROM pastes WHERE id = ?", id)
	}

	return &Entry{
		Meta: meta,
		File: rc,
	}, nil
}

func (s *DBStore) Stat(id string, password string) (Metadata, error) {
	if !validID(id) {
		return Metadata{}, ErrNotFound
	}

	var meta Metadata
	var dbPassHash sql.NullString
	var expiresAtNull sql.NullTime

	query := `
	SELECT id, password_hash, delete_token_hash, created_at, expires_at, data_policy, size, content_type
	FROM pastes
	WHERE id = ?`

	err := s.db.QueryRow(query, id).Scan(
		&meta.ID,
		&dbPassHash,
		&meta.DeleteTokenHash,
		&meta.CreatedAt,
		&expiresAtNull,
		&meta.DataPolicy,
		&meta.Size,
		&meta.ContentType,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Metadata{}, ErrNotFound
		}
		return Metadata{}, err
	}

	if dbPassHash.Valid {
		meta.PasswordHash = dbPassHash.String
	}
	if expiresAtNull.Valid {
		meta.ExpiresAt = expiresAtNull.Time
	}

	if isExpired(meta, time.Now().UTC(), s.TTL) {
		return Metadata{}, ErrNotFound
	}

	if meta.PasswordHash != "" {
		if strings.TrimSpace(password) == "" || !checkSecret(password, meta.PasswordHash) {
			return Metadata{}, ErrInvalidPassword
		}
	}

	return meta, nil
}

func (s *DBStore) Delete(id string, token string) error {
	if !validID(id) {
		return ErrNotFound
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return ErrInvalidDeleteToken
	}

	var deleteTokenHash string
	err := s.db.QueryRow("SELECT delete_token_hash FROM pastes WHERE id = ?", id).Scan(&deleteTokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if deleteTokenHash == "" || !checkSecret(token, deleteTokenHash) {
		return ErrInvalidDeleteToken
	}

	_, err = s.db.Exec("DELETE FROM pastes WHERE id = ?", id)
	return err
}

func (s *DBStore) CleanupExpired() error {
	now := time.Now().UTC()
	cutoff := now.Add(-s.TTL)
	_, err := s.db.Exec(`
		DELETE FROM pastes 
		WHERE (expires_at IS NOT NULL AND expires_at < ?)
		   OR (expires_at IS NULL AND created_at < ?)
		   OR (data_policy = 'permanent' AND created_at < ?)`, 
		now, cutoff, cutoff)
	return err
}

func (s *DBStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *DBStore) List() ([]Metadata, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-s.TTL)
	query := `
	SELECT id, password_hash, delete_token_hash, created_at, expires_at, data_policy, size, content_type
	FROM pastes
	WHERE (expires_at IS NOT NULL AND expires_at >= ?)
	   OR (expires_at IS NULL AND created_at >= ?)
	ORDER BY created_at DESC`

	rows, err := s.db.Query(query, now, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Metadata
	for rows.Next() {
		var meta Metadata
		var dbPassHash sql.NullString
		var expiresAtNull sql.NullTime

		err := rows.Scan(
			&meta.ID,
			&dbPassHash,
			&meta.DeleteTokenHash,
			&meta.CreatedAt,
			&expiresAtNull,
			&meta.DataPolicy,
			&meta.Size,
			&meta.ContentType,
		)
		if err != nil {
			return nil, err
		}

		if dbPassHash.Valid {
			meta.PasswordHash = dbPassHash.String
		}
		if expiresAtNull.Valid {
			meta.ExpiresAt = expiresAtNull.Time
		}

		list = append(list, meta)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return list, nil
}

func (s *DBStore) ForceDelete(id string) error {
	if !validID(id) {
		return ErrNotFound
	}

	_, err := s.db.Exec("DELETE FROM pastes WHERE id = ?", id)
	return err
}

func (s *DBStore) DeleteAll() error {
	_, err := s.db.Exec("DELETE FROM pastes")
	return err
}

// 압축 및 해제 보조 함수들
func compressData(data []byte, algo string) ([]byte, error) {
	switch strings.ToLower(algo) {
	case "zstd":
		var buf bytes.Buffer
		writer, err := zstd.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		_, err = writer.Write(data)
		if err != nil {
			writer.Close()
			return nil, err
		}
		err = writer.Close()
		if err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "gzip":
		var buf bytes.Buffer
		writer := gzip.NewWriter(&buf)
		_, err := writer.Write(data)
		if err != nil {
			writer.Close()
			return nil, err
		}
		err = writer.Close()
		if err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return data, nil
	}
}

type nopReadSeekCloser struct {
	*bytes.Reader
}

func (n *nopReadSeekCloser) Close() error {
	return nil
}

func NewReadSeekCloser(b []byte) io.ReadSeekCloser {
	return &nopReadSeekCloser{bytes.NewReader(b)}
}

func decompressData(data []byte, algo string) (io.ReadSeekCloser, error) {
	switch strings.ToLower(algo) {
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		decompressed, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		return NewReadSeekCloser(decompressed), nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		decompressed, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		return NewReadSeekCloser(decompressed), nil
	default:
		return NewReadSeekCloser(data), nil
	}
}

// LocalStore를 위한 동속성 제어/동기화 구조체
type keyLock struct {
	mu     sync.RWMutex
	refCnt int
}

type lockManager struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

func newLockManager() *lockManager {
	return &lockManager{
		locks: make(map[string]*keyLock),
	}
}

func (lm *lockManager) acquire(key string) (*keyLock, func()) {
	lm.mu.Lock()
	l, ok := lm.locks[key]
	if !ok {
		l = &keyLock{}
		lm.locks[key] = l
	}
	l.refCnt++
	lm.mu.Unlock()

	return l, func() {
		lm.mu.Lock()
		l.refCnt--
		if l.refCnt <= 0 {
			delete(lm.locks, key)
		}
		lm.mu.Unlock()
	}
}

func isExpired(meta Metadata, now time.Time, ttl time.Duration) bool {
	expiresAt := meta.ExpiresAt
	if expiresAt.IsZero() || strings.EqualFold(meta.DataPolicy, "permanent") {
		expiresAt = meta.CreatedAt.Add(ttl)
	}

	return now.After(expiresAt)
}

func validID(id string) bool {
	if len(id) != 5 {
		return false
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
		return false
	}

	return true
}

func hashSecret(secret string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		// Fallback to SHA-256 on unexpected bcrypt failure
		sum := sha256.Sum256([]byte(secret))
		return hex.EncodeToString(sum[:])
	}
	return string(hash)
}

func checkSecret(secret, hash string) bool {
	if strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$") {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
	}
	
	// Legacy SHA-256 Support
	sum := sha256.Sum256([]byte(secret))
	legacyHash := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(legacyHash), []byte(hash)) == 1
}

func generatePassword(length int) (string, error) {
	if length < 4 {
		length = 8
	}

	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lower := "abcdefghijklmnopqrstuvwxyz"
	digits := "0123456789"
	all := upper + lower + digits

	result := make([]byte, 0, length)

	a, err := randomChar(upper)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	a, err = randomChar(lower)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	a, err = randomChar(digits)
	if err != nil {
		return "", err
	}
	result = append(result, a)

	for len(result) < length {
		a, err = randomChar(all)
		if err != nil {
			return "", err
		}
		result = append(result, a)
	}

	if err := shuffleBytes(result); err != nil {
		return "", err
	}

	return string(result), nil
}

func randomChar(alphabet string) (byte, error) {
	n, err := randomIndex(len(alphabet))
	if err != nil {
		return 0, err
	}

	return alphabet[n], nil
}

func randomString(alphabet string, length int) (string, error) {
	result := make([]byte, length)

	for i := range result {
		ch, err := randomChar(alphabet)
		if err != nil {
			return "", err
		}
		result[i] = ch
	}

	return string(result), nil
}

func randomIndex(max int) (int, error) {
	if max <= 0 {
		return 0, errors.New("invalid max")
	}

	var b [1]byte

	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}

		limit := 256 - (256 % max)
		if int(b[0]) < limit {
			return int(b[0]) % max, nil
		}
	}
}

func shuffleBytes(data []byte) error {
	for i := len(data) - 1; i > 0; i-- {
		j, err := randomIndex(i + 1)
		if err != nil {
			return err
		}

		data[i], data[j] = data[j], data[i]
	}

	return nil
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const tokenAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// AlphanumericAlphabet은 외부에서 사용할 수 있는 영숫자 알파벳입니다.
const AlphanumericAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// RandomString은 암호학적으로 안전한 난수 문자열을 생성합니다.
func RandomString(alphabet string, length int) (string, error) {
	return randomString(alphabet, length)
}
