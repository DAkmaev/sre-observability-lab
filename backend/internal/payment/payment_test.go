package payment

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker"

	"sre-lab/backend/internal/resilience"
)

func newTestClient() (*Client, *resilience.Config) {
	cfg := resilience.DefaultConfig()
	client := &Client{
		cfg:        cfg,
		logger:     slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		stateGauge: prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_payment_circuit_breaker_state"}),
		cbMu:       sync.Mutex{},
	}
	client.cb = gobreaker.NewCircuitBreaker(client.buildSettings())
	return client, cfg
}

func boolPtr(b bool) *bool { return &b }

func TestRunProtectedBypassesWhenDisabled(t *testing.T) {
	client, cfg := newTestClient()

	for i := 0; i < 5; i++ {
		_, _ = client.cb.Execute(func() (interface{}, error) { return nil, errors.New("fail") })
	}
	if client.cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected breaker OPEN, got %s", client.cb.State())
	}

	cfg.Set(resilience.Patch{CircuitBreakerEnabled: boolPtr(false)})
	var calls int
	got, err := client.runProtected(func() (interface{}, error) {
		calls++
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %v, want ok", got)
	}
	if calls != 1 {
		t.Fatalf("expected fn called exactly once, got %d", calls)
	}
	if client.State() != "disabled" {
		t.Fatalf("expected State() disabled, got %q", client.State())
	}
}

func TestRunProtectedUsesBreakerWhenEnabled(t *testing.T) {
	client, _ := newTestClient()
	for i := 0; i < 5; i++ {
		_, _ = client.cb.Execute(func() (interface{}, error) { return nil, errors.New("fail") })
	}
	if client.cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected breaker OPEN, got %s", client.cb.State())
	}

	var calls int
	_, err := client.runProtected(func() (interface{}, error) {
		calls++
		return "ok", nil
	})
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("expected ErrOpenState, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected fn not called when OPEN, got %d calls", calls)
	}
}

func TestResetCircuitBreakerReturnsClosed(t *testing.T) {
	client, _ := newTestClient()
	for i := 0; i < 5; i++ {
		_, _ = client.cb.Execute(func() (interface{}, error) { return nil, errors.New("fail") })
	}
	if client.cb.State() != gobreaker.StateOpen {
		t.Fatalf("precondition: expected breaker OPEN, got %s", client.cb.State())
	}

	client.ResetCircuitBreaker()
	if client.cb.State() != gobreaker.StateClosed {
		t.Fatalf("expected CLOSED after reset, got %s", client.cb.State())
	}
	if client.State() != gobreaker.StateClosed.String() {
		t.Fatalf("expected State() closed, got %q", client.State())
	}
}
