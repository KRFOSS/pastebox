package internal

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestStoreBasic(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pastebox-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewLocalStore(tempDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	content := []byte("hello world")
	meta, password, deleteToken, err := store.Create(bytes.NewReader(content), "text/plain", true)
	if err != nil {
		t.Fatalf("failed to create entry: %v", err)
	}

	if meta.ID == "" {
		t.Error("expected non-empty ID")
	}
	if password == "" {
		t.Error("expected generated password")
	}
	if deleteToken == "" {
		t.Error("expected delete token")
	}

	// 잘못된 비밀번호로 조회 시도
	_, err = store.Open(meta.ID, "wrong-password")
	if err != ErrInvalidPassword {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}

	// 올바른 비밀번호로 조회 시도
	entry, err := store.Open(meta.ID, password)
	if err != nil {
		t.Fatalf("failed to open entry: %v", err)
	}
	defer entry.File.Close()

	readBuf, err := io.ReadAll(entry.File)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if !bytes.Equal(readBuf, content) {
		t.Errorf("expected %q, got %q", content, readBuf)
	}

	// 삭제
	err = store.Delete(meta.ID, deleteToken)
	if err != nil {
		t.Fatalf("failed to delete entry: %v", err)
	}

	// 삭제된 건 조회 시도
	_, err = store.Open(meta.ID, password)
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound for deleted entry, got %v", err)
	}
}

func TestStoreConcurrency(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pastebox-concurrency-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewLocalStore(tempDir, 10*time.Millisecond) // 짧은 만료 기한
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup

	// 동시성 생성, 조회, 삭제 수행
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gId int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// 1. 생성
				content := []byte("concurrency test data")
				meta, password, deleteToken, err := store.Create(bytes.NewReader(content), "text/plain", true)
				if err != nil {
					t.Errorf("[g:%d, i:%d] Create failed: %v", gId, j, err)
					return
				}

				// 2. 동시에 여러 번 조회
				var openWg sync.WaitGroup
				for k := 0; k < 5; k++ {
					openWg.Add(1)
					go func() {
						defer openWg.Done()
						entry, err := store.Open(meta.ID, password)
						if err == nil {
							entry.File.Close()
						}
					}()
				}
				openWg.Wait()

				// 3. 삭제
				if j%2 == 0 {
					err = store.Delete(meta.ID, deleteToken)
					if err != nil && err != ErrNotFound {
						t.Errorf("[g:%d, i:%d] Delete failed: %v", gId, j, err)
					}
				}
			}
		}(i)
	}

	// 동시에 만료 정리(CleanupExpired) 처리
	stopCleanup := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopCleanup:
				return
			default:
				_ = store.CleanupExpired()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	wg.Wait()
	close(stopCleanup)
}

func TestStoreAdmin(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pastebox-admin-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewLocalStore(tempDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// 1. Create two pastes
	meta1, _, _, err := store.Create(bytes.NewReader([]byte("paste 1")), "text/plain", false)
	if err != nil {
		t.Fatalf("failed to create entry 1: %v", err)
	}
	meta2, _, _, err := store.Create(bytes.NewReader([]byte("paste 2")), "text/plain", false)
	if err != nil {
		t.Fatalf("failed to create entry 2: %v", err)
	}

	// 2. List pastes and verify
	list, err := store.List()
	if err != nil {
		t.Fatalf("failed to list pastes: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 pastes, got %d", len(list))
	}

	// 3. ForceDelete one paste
	err = store.ForceDelete(meta1.ID)
	if err != nil {
		t.Fatalf("failed to force delete paste 1: %v", err)
	}

	// Check listing again
	list, err = store.List()
	if err != nil {
		t.Fatalf("failed to list pastes after delete: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 paste, got %d", len(list))
	}
	if list[0].ID != meta2.ID {
		t.Errorf("expected remaining paste to be %s, got %s", meta2.ID, list[0].ID)
	}

	// 4. DeleteAll pastes
	err = store.DeleteAll()
	if err != nil {
		t.Fatalf("failed to delete all: %v", err)
	}

	list, err = store.List()
	if err != nil {
		t.Fatalf("failed to list pastes after delete all: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 pastes, got %d", len(list))
	}
}
