// Package resilience содержит учебные реализации паттернов отказоустойчивости
// для повышения устойчивости сервисов:
//
//   - Retry    — повтор временных сбоев с экспоненциальной задержкой.
//   - Timeout  — явное ограничение времени на запрос к зависимости.
//   - Bulkhead — изоляция ресурсов: лимит одновременных запросов к одной
//     зависимости, чтобы перегрузка одного вызова не съела все ресурсы.
//   - Fallback — возврат запасного результата (кэш последнего успеха) при
//     отказе зависимости вместо того, чтобы молча отдавать ошибку.
//
// Каждый паттерн управляется на лету через конфиг (см. Config) и даёт свои
// метрики/логи, чтобы его состояние было видно,
// как уже показываются circuit breaker и rate limit.
package resilience

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// ── Retry ─────────────────────────────────────────────────────────────────

// RetryConfig описывает параметры паттерна Retry.
type RetryConfig struct {
	// MaxRetries — сколько раз повторить запрос (помимо первой попытки).
	MaxRetries int
	// BaseDelay — начальная задержка, которая растёт экспоненциально.
	BaseDelay time.Duration
	// MaxDelay — потолок экспоненциальной задержки.
	MaxDelay time.Duration
}

// Retry выполняет fn, повторяя его на временных ошибках. Между попытками —
// экспоненциальная задержка с джиттером. Возвращает результат последней
// (успешной или неуспешной) попытки, число сделанных попыток (>=1) и ошибку.
func Retry[T any](ctx context.Context, cfg RetryConfig, retryable IsRetryable, logger *slog.Logger, name string, fn func() (T, error)) (T, int, error) {
	var zero T
	if cfg.MaxRetries <= 0 {
		v, err := fn()
		return v, 1, err
	}

	delay := cfg.BaseDelay
	var lastErr error
	attempts := 0
	for attempts <= cfg.MaxRetries {
		if attempts > 0 {
			// Дожидаемся следующей попытки, но прекращаем, если контекст отменён.
			select {
			case <-ctx.Done():
				return zero, attempts, ctx.Err()
			case <-time.After(delay):
			}
			if logger != nil {
				logger.Warn("retry_attempt", "dependency", name, "attempt", attempts, "delay_ms", delay.Milliseconds(), "error", lastErr.Error())
			}
		}
		attempts++
		v, err := fn()
		if err == nil {
			return v, attempts, nil
		}
		lastErr = err
		if !retryable(err) {
			return zero, attempts, err
		}
		if delay < cfg.MaxDelay {
			delay = backoffDelay(delay*2, cfg.MaxDelay)
		}
	}
	return zero, attempts, lastErr
}

// backoffDelay увеличивает задержку, но не больше max, с небольшим случайным
// джиттером (±20%), чтобы одновременные клиенты не синхронизировали ретраи
// (эффект лавины повторных запросов).
func backoffDelay(d, max time.Duration) time.Duration {
	if d > max {
		d = max
	}
	jitter := time.Duration(float64(d) * 0.2 * (2*rand.Float64() - 1))
	return d + jitter
}

// ── Timeout ───────────────────────────────────────────────────────────────

// DoWithTimeout выполняет fn с жёстким дедлайном. Если fn не успел уложиться,
// возвращается ошибка context.DeadlineExceeded — защита от "зависшего" вызова
// (ограниченное время ожидания).
func DoWithTimeout[T any](ctx context.Context, timeout time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	if timeout <= 0 {
		return fn(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(ctx)
}

// ── Bulkhead ──────────────────────────────────────────────────────────────

// ErrBulkheadFull — локальный быстрый отказ, когда все "перегородки" заняты.
// Это и есть суть bulkhead: вызов НЕ уходит в сеть, если изолированный пул
// ресурсов зависимость исчерпан — перегрузка одного трейса не должна ронять
// соседние.
var ErrBulkheadFull = errors.New("bulkhead: dependency has no free slots")

// Bulkhead ограничивает число одновременных вызовов к одной зависимости.
// Semaphore — канал-счётчик, учебная замена для golang.org/x/sync/semaphore.
type Bulkhead struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	inUse int
}

// NewBulkhead создаёт перегородку на limit одновременных запросов.
func NewBulkhead(limit int) *Bulkhead {
	b := &Bulkhead{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// SetLimit меняет размер перегородки на лету (настраиваемый через админ-панель).
func (b *Bulkhead) SetLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	b.mu.Lock()
	b.limit = limit
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Limit возвращает текущий размер перегородки.
func (b *Bulkhead) Limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Acquire занимает слот. Если все слоты заняты, сразу возвращается
// ErrBulkheadFull — быстрый локальный отказ без ожидания (fail fast).
func (b *Bulkhead) Acquire() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse >= b.limit {
		return ErrBulkheadFull
	}
	b.inUse++
	return nil
}

// Done освобождает слот.
func (b *Bulkhead) Done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inUse--
	b.cond.Broadcast()
}

// ── Fallback ──────────────────────────────────────────────────────────────

// Fallback — кэш последнего успешного результата с TTL. Когда живой вызов
// падает, возвращаем запасной (возможно устаревший) результат — это тот
// кэшированные данные механизма fallback.
type Fallback[T any] struct {
	mu    sync.RWMutex
	ttl   time.Duration
	value *T
	time  *time.Time
}

// NewFallback создаёт кэш-фолбэк с TTL. Если ttl<=0, кэш живёт вечно.
func NewFallback[T any](ttl time.Duration) *Fallback[T] {
	return &Fallback[T]{ttl: ttl}
}

// SetTTL меняет время жизни кэша на лету (настраиваемый через админ-панель).
func (f *Fallback[T]) SetTTL(ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttl = ttl
}

// Remember запоминает успешный результат как кандидата для фолбэка.
func (f *Fallback[T]) Remember(value T) {
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.value = &value
	f.time = &now
}

// Get отдаёт запасное значение, если оно есть и не устарело. ok=false, если
// фолбэка нет вообще (например, зависимость падает с самого начала и успеха
// ещё не было).
func (f *Fallback[T]) Get() (T, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var zero T
	if f.value == nil {
		return zero, false
	}
	if f.ttl > 0 && f.time != nil && time.Since(*f.time) > f.ttl {
		return zero, false
	}
	return *f.value, true
}
