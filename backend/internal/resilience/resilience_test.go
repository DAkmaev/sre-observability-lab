package resilience

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// ── Retry ──────────────────────────────────────────────────────────────────

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	var calls atomic.Int32
	cfg := RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	fn := func() (int, error) {
		if calls.Add(1) < 3 {
			return 0, errors.New("boom")
		}
		return 42, nil
	}

	got, attempts, err := Retry(context.Background(), cfg, func(error) bool { return true }, nil, "test", fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestRetryNoRetryWhenDisabled(t *testing.T) {
	var calls atomic.Int32
	cfg := RetryConfig{MaxRetries: 0}
	fn := func() (int, error) {
		calls.Add(1)
		return 0, errors.New("boom")
	}
	_, attempts, err := Retry(context.Background(), cfg, func(error) bool { return true }, nil, "test", fn)
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt when MaxRetries=0, got %d", attempts)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls.Load())
	}
}

func TestRetryDoesNotRetryNonRetryable(t *testing.T) {
	var calls atomic.Int32
	cfg := RetryConfig{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	fn := func() (int, error) {
		calls.Add(1)
		return 0, errors.New("permanent")
	}
	_, _, err := Retry(context.Background(), cfg, func(e error) bool { return false }, nil, "test", fn)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("non-retryable error should be attempted once, got %d", calls.Load())
	}
}

func TestRetryRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // уже отменён — первая же попытка должна прерваться до ретрая

	cfg := RetryConfig{MaxRetries: 5, BaseDelay: time.Hour, MaxDelay: time.Hour}
	var calls atomic.Int32
	fn := func() (int, error) {
		calls.Add(1)
		return 0, errors.New("boom")
	}
	_, _, err := Retry(ctx, cfg, func(error) bool { return true }, nil, "test", fn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// ── Bulkhead ───────────────────────────────────────────────────────────────

func TestBulkheadRejectsWhenFullAndReleases(t *testing.T) {
	b := NewBulkhead(2)

	if err := b.Acquire(); err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	if err := b.Acquire(); err != nil {
		t.Fatalf("acquire 2 failed: %v", err)
	}
	if err := b.Acquire(); !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("expected ErrBulkheadFull, got %v", err)
	}

	b.Done() // освобождаем один слот
	if err := b.Acquire(); err != nil {
		t.Fatalf("acquire after Done failed: %v", err)
	}
}

func TestBulkheadSetLimit(t *testing.T) {
	b := NewBulkhead(1)
	b.SetLimit(2)
	if b.Limit() != 2 {
		t.Fatalf("limit = %d, want 2", b.Limit())
	}
}

// ── Fallback ───────────────────────────────────────────────────────────────

func TestFallbackRememberAndGet(t *testing.T) {
	f := NewFallback[int](time.Minute)
	if _, ok := f.Get(); ok {
		t.Fatal("empty fallback should not return a value")
	}
	f.Remember(7)
	v, ok := f.Get()
	if !ok || v != 7 {
		t.Fatalf("got %d/%v, want 7/true", v, ok)
	}
}

func TestFallbackExpires(t *testing.T) {
	f := NewFallback[int](5 * time.Millisecond)
	f.Remember(7)
	time.Sleep(15 * time.Millisecond)
	if _, ok := f.Get(); ok {
		t.Fatal("expired fallback value should not be returned")
	}
}

// ── ProtectedCall ──────────────────────────────────────────────────────────

func TestProtectedCallServesFallbackOnFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Set(Patch{FallbackEnabled: boolPtr(true), FallbackTTLMs: intPtr(30000)})
	m := NewMetrics()
	fb := NewFallback[*testPayload](time.Minute)
	bulkhead := NewBulkhead(4)

	fb.Remember(&testPayload{val: "cached"})

	fn := func(context.Context) (*testPayload, error) {
		return nil, errors.New("dependency down")
	}

	got, out, err := ProtectedCall(context.Background(), "test", cfg, m, fb, bulkhead, nil,
		func(e error) bool { return false }, fn)
	if err != nil {
		t.Fatalf("expected nil error via fallback, got %v", err)
	}
	if got == nil || got.val != "cached" {
		t.Fatalf("expected cached payload, got %+v", got)
	}
	if out == nil || !out.FallbackHit {
		t.Fatal("expected FallbackHit=true")
	}
}

func TestProtectedCallBulkheadFailFast(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Set(Patch{BulkheadEnabled: boolPtr(true), BulkheadLimit: intPtr(1)})
	m := NewMetrics()
	bulkhead := NewBulkhead(1)
	_ = bulkhead.Acquire() // занимаем единственный слот — перегородка полна

	_, out, err := ProtectedCall(context.Background(), "test", cfg, m, nil, bulkhead, nil,
		func(e error) bool { return false }, func(context.Context) (*testPayload, error) {
			return &testPayload{val: "x"}, nil
		})
	if !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("expected ErrBulkheadFull, got %v", err)
	}
	if out == nil || !out.BulkheadRej {
		t.Fatal("expected BulkheadRej=true")
	}
}

func TestProtectedCallTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Set(Patch{TimeoutEnabled: boolPtr(true), TimeoutMs: intPtr(20)})
	m := NewMetrics()

	_, out, err := ProtectedCall(context.Background(), "test", cfg, m, nil, nil, nil,
		func(e error) bool { return false },
		func(ctx context.Context) (*testPayload, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
				return &testPayload{val: "late"}, nil
			}
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if out == nil || !out.TimedOut {
		t.Fatal("expected TimedOut=true")
	}
}

func TestProtectedCallRemembersSuccessForFallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Set(Patch{FallbackEnabled: boolPtr(true)})
	m := NewMetrics()
	fb := NewFallback[*testPayload](time.Minute)

	got, _, err := ProtectedCall(context.Background(), "test", cfg, m, fb, nil, nil,
		func(e error) bool { return false },
		func(context.Context) (*testPayload, error) {
			return &testPayload{val: "fresh"}, nil
		})
	if err != nil || got.val != "fresh" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	v, ok := fb.Get()
	if !ok || v.val != "fresh" {
		t.Fatalf("expected fallback to remember fresh value, got %+v ok=%v", v, ok)
	}
}

type testPayload struct {
	val string
}

func boolPtr(b bool) *bool { return &b }

func intPtr(i int) *int { return &i }
