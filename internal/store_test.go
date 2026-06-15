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
	meta, password, deleteToken, err := store.Create(bytes.NewReader(content), "text/plain", true, "temporary", 0)
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

	weekMeta, _, _, err := store.Create(bytes.NewReader(content), "text/plain", false, "week", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("failed to create week entry: %v", err)
	}
	if weekMeta.DataPolicy != "week" {
		t.Fatalf("expected week policy, got %q", weekMeta.DataPolicy)
	}
	expectedWeekExpiresAt := weekMeta.CreatedAt.Add(7 * 24 * time.Hour)
	if weekMeta.ExpiresAt.Before(expectedWeekExpiresAt.Add(-time.Second)) || weekMeta.ExpiresAt.After(expectedWeekExpiresAt.Add(time.Second)) {
		t.Fatalf("expected week expiry near %s, got %s", expectedWeekExpiresAt, weekMeta.ExpiresAt)
	}

	permanentMeta, _, _, err := store.Create(bytes.NewReader(content), "text/plain", false, "permanent", 0)
	if err != nil {
		t.Fatalf("failed to create permanent entry: %v", err)
	}
	if permanentMeta.DataPolicy != "permanent" {
		t.Fatalf("expected permanent policy, got %q", permanentMeta.DataPolicy)
	}
	if !permanentMeta.ExpiresAt.IsZero() {
		t.Fatalf("expected permanent entry to store zero expiry, got %s", permanentMeta.ExpiresAt)
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
	entry.File.Close()
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

	store, err := NewLocalStore(tempDir, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gId int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				content := []byte("concurrency test data")
				meta, password, deleteToken, err := store.Create(bytes.NewReader(content), "text/plain", true, "temporary", 0)
				if err != nil {
					t.Errorf("[g:%d, i:%d] Create failed: %v", gId, j, err)
					return
				}

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

				if j%2 == 0 {
					err = store.Delete(meta.ID, deleteToken)
					if err != nil && err != ErrNotFound {
						t.Errorf("[g:%d, i:%d] Delete failed: %v", gId, j, err)
					}
				}
			}
		}(i)
	}

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

	meta1, _, _, err := store.Create(bytes.NewReader([]byte("paste 1")), "text/plain", false, "temporary", 0)
	if err != nil {
		t.Fatalf("failed to create entry 1: %v", err)
	}
	meta2, _, _, err := store.Create(bytes.NewReader([]byte("paste 2")), "text/plain", false, "temporary", 0)
	if err != nil {
		t.Fatalf("failed to create entry 2: %v", err)
	}

	list, _, err := store.List("created_at", "desc", 0, 10)
	if err != nil {
		t.Fatalf("failed to list pastes: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 pastes, got %d", len(list))
	}

	err = store.ForceDelete(meta1.ID)
	if err != nil {
		t.Fatalf("failed to force delete paste 1: %v", err)
	}

	list, _, err = store.List("created_at", "desc", 0, 10)
	if err != nil {
		t.Fatalf("failed to list pastes after delete: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 paste, got %d", len(list))
	}
	if list[0].ID != meta2.ID {
		t.Errorf("expected remaining paste to be %s, got %s", meta2.ID, list[0].ID)
	}

	err = store.DeleteAll()
	if err != nil {
		t.Fatalf("failed to delete all: %v", err)
	}

	list, _, err = store.List("created_at", "desc", 0, 10)
	if err != nil {
		t.Fatalf("failed to list pastes after delete all: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 pastes, got %d", len(list))
	}
}
