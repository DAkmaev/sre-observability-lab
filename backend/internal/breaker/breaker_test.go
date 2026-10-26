package breaker

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sony/gobreaker"

	"sre-lab/backend/internal/resilience"
)

// newTestClient собирает клиента напрямую, без promauto/метрик, чтобы тесты
// не зависели от глобального реестра и не падали на дубликатах gauge.
// Короткий Timeout позволяет проверить полный жизненный цикл breaker
// (open → half-open → closed) не дожидаясь реальных 10 секунд.
func newTestClient() *RecommendationsClient {
	cfg := resilience.DefaultConfig()
	settings := gobreaker.Settings{
		Name:        "recommendations",
		MaxRequests: 1,
		Interval:    0,
		Timeout:     200 * time.Millisecond,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		},
	}
	return &RecommendationsClient{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		cb:     gobreaker.NewCircuitBreaker(settings),
	}
}

// tripOpen подаёт в breaker подряд столько ошибок, пока он не разомкнётся.
func tripOpen(t *testing.T, cb *gobreaker.CircuitBreaker) {
	t.Helper()
	for i := 0; i < 20; i++ {
		_, _ = cb.Execute(func() (interface{}, error) { return nil, errors.New("fail") })
		if cb.State() == gobreaker.StateOpen {
			return
		}
	}
	t.Fatalf("expected breaker to open, but stayed %s", cb.State())
}

func TestBreakerTripsOpenAndDropsRequests(t *testing.T) {
	client := newTestClient()
	tripOpen(t, client.cb)

	if client.cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected breaker OPEN, got %s", client.cb.State())
	}

	// Пока цепь разомкнута, подзадача не должна вызываться вовсе, а сам
	// вызов обязан вернуть ErrOpenState.
	var calls int
	_, err := client.cb.Execute(func() (interface{}, error) {
		calls++
		return "ok", nil
	})
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("expected ErrOpenState, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected fn not called when OPEN, got %d calls", calls)
	}
	if client.State() != gobreaker.StateOpen.String() {
		t.Fatalf("expected State() open, got %q", client.State())
	}
}

func TestBreakerRecoversAfterTimeout(t *testing.T) {
	client := newTestClient()
	tripOpen(t, client.cb)

	// После истечения Timeout breaker переходит в half-open и пропускает
	// ровно один пробный запрос (MaxRequests: 1).
	time.Sleep(250 * time.Millisecond)

	got, err := client.cb.Execute(func() (interface{}, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("unexpected error after recovery: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %v, want ok", got)
	}
	if client.cb.State() != gobreaker.StateClosed {
		t.Fatalf("expected CLOSED after successful probe, got %s", client.cb.State())
	}
}
