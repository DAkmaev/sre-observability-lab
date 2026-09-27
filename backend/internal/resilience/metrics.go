package resilience

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics хранит Prometheus-метрики паттернов отказоустойчивости. Создаётся
// один раз (на процесс backend), а разные зависимости пишут в свой лейбл
// "dependency" (recommendations / payment), чтобы на дашборде было видно,
// какой паттерн что сделал для какой зависимости.
type Metrics struct {
	// RetryAttempts — сколько повторных попыток сделано (суммарно).
	RetryAttempts *prometheus.CounterVec
	// RetryExhausted — сколько раз ретраи исчерпаны (зависимость так и не ответила).
	RetryExhausted *prometheus.CounterVec
	// BulkheadRejections — сколько запросов отклонено перегородкой (fail fast).
	BulkheadRejections *prometheus.CounterVec
	// FallbackHits — сколько раз отдан кэшированный результат вместо живого вызова.
	FallbackHits *prometheus.CounterVec
	// TimeoutExceeded — сколько раз зависимость превысила заданный таймаут.
	TimeoutExceeded *prometheus.CounterVec
}

// NewMetrics создаёт и регистрирует метрики паттернов. Повторная передача в
// глобальный реестр Prometheus (например, между test-кейсами или при
// пересоздании в тестах) безопасно игнорируется.
func NewMetrics() *Metrics {
	return &Metrics{
		RetryAttempts:      mustRegisterCounter("resilience_retries_total", "Количество повторных попыток (Retry) по зависимости.", "dependency"),
		RetryExhausted:     mustRegisterCounter("resilience_retry_exhausted_total", "Ретраи исчерпаны: зависимость так и не ответила успехом.", "dependency"),
		BulkheadRejections: mustRegisterCounter("resilience_bulkhead_rejections_total", "Запросы, отклонённые перегородкой (bulkhead) — быстрый локальный отказ.", "dependency"),
		FallbackHits:       mustRegisterCounter("resilience_fallback_hits_total", "Сколько раз отдан кэш последнего успеха вместо живого вызова (Fallback).", "dependency"),
		TimeoutExceeded:    mustRegisterCounter("resilience_timeout_exceeded_total", "Сколько раз запрос к зависимости превысил таймаут.", "dependency"),
	}
}

func mustRegisterCounter(name, help, label string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "backend",
		Name:      name,
		Help:      help,
	}, []string{label})
	register(c)
	return c
}

// register регистрирует коллектор, молча игнорируя уже зарегистрированный
// (случается в тестах, когда NewMetrics вызывается несколько раз).
func register(c prometheus.Collector) {
	if err := prometheus.Register(c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return
		}
		panic(err)
	}
}

func (m *Metrics) retryAttempt(dep string) {
	if m == nil || m.RetryAttempts == nil {
		return
	}
	m.RetryAttempts.WithLabelValues(dep).Inc()
}

func (m *Metrics) retryExhausted(dep string) {
	if m == nil || m.RetryExhausted == nil {
		return
	}
	m.RetryExhausted.WithLabelValues(dep).Inc()
}

func (m *Metrics) bulkheadReject(dep string) {
	if m == nil || m.BulkheadRejections == nil {
		return
	}
	m.BulkheadRejections.WithLabelValues(dep).Inc()
}

func (m *Metrics) fallbackHit(dep string) {
	if m == nil || m.FallbackHits == nil {
		return
	}
	m.FallbackHits.WithLabelValues(dep).Inc()
}

func (m *Metrics) timeoutExceeded(dep string) {
	if m == nil || m.TimeoutExceeded == nil {
		return
	}
	m.TimeoutExceeded.WithLabelValues(dep).Inc()
}
