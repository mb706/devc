package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquire(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "lock")
	release, err := Acquire(ctx, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	_, err = Acquire(ctx, p, Options{Timeout: 150 * time.Millisecond, OnWait: func() { waited = true }})
	if !errors.Is(err, ErrTimeout) || !waited {
		t.Errorf("second exclusive lock: %v (waited %v)", err, waited)
	}
	if _, err := Acquire(ctx, p, Options{Shared: true, Timeout: 100 * time.Millisecond}); !errors.Is(err, ErrTimeout) {
		t.Errorf("shared lock while held exclusively: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Acquire(cctx, p, Options{Timeout: time.Minute}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled wait: %v", err)
	}
	// The lock becomes free while a waiter polls.
	go func() { time.Sleep(100 * time.Millisecond); release() }()
	r2, err := Acquire(ctx, p, Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	r2()
	// Shared locks coexist.
	s1, err1 := Acquire(ctx, p, Options{Shared: true})
	s2, err2 := Acquire(ctx, p, Options{Shared: true})
	if err1 != nil || err2 != nil {
		t.Fatalf("shared locks: %v %v", err1, err2)
	}
	s1()
	s2()
	// A symlink in place of the lock file is not followed.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(ctx, link, Options{}); err == nil {
		t.Error("lock through a symlink succeeded")
	}
}
