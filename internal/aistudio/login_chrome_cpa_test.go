package aistudio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForDevToolsActivePortWithGrace_DeElevatedChildReadyAfterLauncherExit(t *testing.T) {
	userDataDir := t.TempDir()
	var exited atomic.Bool
	waitDone := make(chan struct{})

	// Simulate elevated Windows Chrome exiting right after spawning de-elevated child
	exited.Store(true)
	close(waitDone)

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(userDataDir, "DevToolsActivePort"), []byte("54321\n/devtools/browser/abc-123\n"), 0o600)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	port, detached, err := waitForDevToolsActivePortWithGrace(ctx, userDataDir, &exited, waitDone, func() error { return nil }, 2*time.Second)
	if err != nil {
		t.Fatalf("expected de-elevated browser port to succeed after launcher exit, got error: %v", err)
	}
	if port != 54321 {
		t.Fatalf("expected port 54321, got %d", port)
	}
	if !detached {
		t.Fatalf("expected detached=true when launcher already exited before DevToolsActivePort appeared")
	}
}

func TestWaitForDevToolsActivePortWithGrace_FailsWhenNeverReady(t *testing.T) {
	userDataDir := t.TempDir()
	var exited atomic.Bool
	waitDone := make(chan struct{})
	exitErr := errors.New("exit status 1")

	exited.Store(true)
	close(waitDone)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := waitForDevToolsActivePortWithGrace(ctx, userDataDir, &exited, waitDone, func() error { return exitErr }, 250*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when launcher exits and DevToolsActivePort never appears")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("expected error to include exit detail, got: %v", err)
	}
}

func TestWaitForDevToolsActivePortWithGrace_DirectChildReady(t *testing.T) {
	userDataDir := t.TempDir()
	var exited atomic.Bool
	waitDone := make(chan struct{})

	if err := os.WriteFile(filepath.Join(userDataDir, "DevToolsActivePort"), []byte("61234\n/devtools/browser/xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	port, detached, err := waitForDevToolsActivePortWithGrace(ctx, userDataDir, &exited, waitDone, nil, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != 61234 || detached {
		t.Fatalf("expected port=61234, detached=false, got port=%d detached=%v", port, detached)
	}
}
