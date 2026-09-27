package resilience

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Outcome описывает, какие паттерны сработали во время одного учебного вызова.
// Используется для логов/трейсов и даёт студентам "видимый след" того, что
// происходило внутри защиты.
type Outcome struct {
	Retried     bool          // была хотя бы одна повторная попытка
	Retries     int           // сколько именно повторов
	TotalTime   time.Duration // суммарное время всех попыток
	BulkheadRej bool          // запрос отклонён перегородкой (fail fast)
	FallbackHit bool          // отдан кэшированный результат
	TimedOut    bool          // зависимость не уложилась в таймаут
	FallbackTTL time.Duration
}

// Any говорит, сработал ли хоть один учебный паттерн во время вызова.
func (o *Outcome) Any() bool {
	return o.Retried || o.BulkheadRej || o.FallbackHit || o.TimedOut
}

// ProtectedCall оборачивает реальный вызов зависимости во все включённые
// паттерны в разумном порядке:
//
//	(fallback slot) → bulkhead (fail fast) → retry → (timeout) → call
//
// При успехе результат запоминается в кэш фолбэка. При итоговой ошибке, если
// фолбэк включён и в кэше есть свежий результат, — отдаём его, помечая
// FallbackHit. retryable — предикат, который решает, что для ЭТОЙ зависимости
// является временным сбоем (его предоставляет конкретный клиент: например,
// для recommendations — 5xx/сеть, но не 429 и не бизнес-отказ).
func ProtectedCall[T any](
	ctx context.Context,
	dep string,
	cfg *Config,
	m *Metrics,
	fb *Fallback[T],
	bulkhead *Bulkhead,
	logger *slog.Logger,
	retryable IsRetryable,
	fn func(ctx context.Context) (T, error),
) (T, *Outcome, error) {
	var zero T
	out := &Outcome{}
	fallbackEnabled, fallbackTTL := cfg.FallbackSettings()
	if fb != nil && fallbackEnabled {
		out.FallbackTTL = fallbackTTL
		fb.SetTTL(fallbackTTL)
	}

	// 1. Bulkhead: не пускаем в сеть больше, чем разрешает перегородка.
	bhEnabled, bhLimit := cfg.BulkheadSettings()
	if bhEnabled && bulkhead != nil {
		bulkhead.SetLimit(bhLimit)
		if err := bulkhead.Acquire(); err != nil {
			out.BulkheadRej = true
			m.bulkheadReject(dep)
			if logger != nil {
				logger.Warn("bulkhead_rejected", "dependency", dep, "limit", bulkhead.Limit())
			}
			return zero, out, ErrBulkheadFull
		}
		defer bulkhead.Done()
	}

	// 2. Таймаут оборачивает собственно вызов.
	timeoutEnabled, timeout := cfg.HasTimeout()

	// 3. Ретрай + сам вызов (под таймаутом, если он включён).
	retryCfg := cfg.Retry()
	start := time.Now()
	var value T
	var err error
	var attempts int

	run := func(ctx context.Context) (T, error) {
		if timeoutEnabled {
			v, e := DoWithTimeout(ctx, timeout, fn)
			if errors.Is(e, context.DeadlineExceeded) {
				m.timeoutExceeded(dep)
				out.TimedOut = true
			}
			return v, e
		}
		return fn(ctx)
	}

	value, attempts, err = Retry(ctx, retryCfg, retryable, logger, dep, func() (T, error) {
		return run(ctx)
	})
	out.TotalTime = time.Since(start)
	out.Retries = attempts - 1
	out.Retried = attempts > 1

	if err != nil {
		if out.Retried {
			m.retryExhausted(dep)
		}
		// 4. Fallback: живой вызов провалился (после всех ретраев/таймаута) —
		// пробуем отдать кэш последнего успеха.
		if fb != nil && fallbackEnabled {
			if v, ok := fb.Get(); ok {
				m.fallbackHit(dep)
				out.FallbackHit = true
				return v, out, nil
			}
		}
		return zero, out, err
	}

	if out.Retried {
		m.retryAttempt(dep)
	}
	// 5. Успех — запоминаем как кандидата фолбэка.
	if fb != nil && fallbackEnabled {
		fb.Remember(value)
	}
	return value, out, nil
}

// IsRetryable — предикат, решающий, стоит ли повторять именно эту ошибку.
// Каждый клиент зависимости задаёт свою семантику.
type IsRetryable = func(err error) bool
