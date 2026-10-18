// Package obs собирает метрики, логи и трейсы в одном месте:
// трейсинг (OpenTelemetry → Tempo), структурированные логи (slog JSON →
// stdout, откуда их забирает Vector) и RED-метрики (Prometheus →
// VictoriaMetrics).
package obs

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ── Трейсинг ────────────────────────────────────────────────────────────

// InitTracing поднимает OpenTelemetry SDK и отправляет спаны в Tempo по
// OTLP/HTTP. otlpEndpoint — это host:port без схемы, например "tempo:4318".
func InitTracing(ctx context.Context, serviceName, otlpEndpoint string) (func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otlpEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// W3C traceparent — именно этот заголовок передаёт trace_id между
	// сервисами (распространение контекста трейса).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// ── Логирование ─────────────────────────────────────────────────────────

// NewLogger создаёт JSON-логгер (stdout), который потом собирает Vector
// и отправляет в VictoriaLogs — для централизованного сбора логов.
func NewLogger(serviceName string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler).With("service", serviceName)
}

// WithTrace добавляет trace_id/span_id из контекста в логгер — это то, что
// связывает логи с трейсами.
func WithTrace(ctx context.Context, l *slog.Logger) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return l
	}
	return l.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}

// ── Метрики (RED: Rate, Errors, Duration — формат RED) ────────────────

type Metrics struct {
	RequestsTotal        *prometheus.CounterVec
	RequestDuration      *prometheus.HistogramVec
	RateLimitRejections  prometheus.Counter
	RateLimitConfiguredR prometheus.Gauge
}

func NewMetrics(namespace string) *Metrics {
	return &Metrics{
		RequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests (Rate + Errors из RED).",
		}, []string{"method", "route", "status"}),

		RequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Duration of HTTP requests (Duration из RED). Смотрим p95/p99, не среднее.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),

		RateLimitRejections: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rate_limit_rejections_total",
			Help:      "Requests rejected by the recommendations rate limiter.",
		}),
		RateLimitConfiguredR: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "rate_limit_requests_per_second",
			Help:      "Configured recommendations rate limit; 0 means disabled.",
		}),
	}
}

// ── HTTP middleware: request-id, метрики, логирование ───────────────────

type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID кладёт request id (из заголовка X-Request-ID, проставленного
// gateway correlation ID) в контекст запроса.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = "unknown"
		}
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return "unknown"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Instrument оборачивает конкретный хендлер: считает RED-метрики и пишет
// access-лог с привязкой к route-шаблону (а не к сырому пути — иначе
// {id}=1,2,3... даст кардинальный взрыв в лейблах, ровно то, о чём
// помогает выбирать метрики и SLI).
func Instrument(m *Metrics, logger *slog.Logger, method, route string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		duration := time.Since(start).Seconds()
		status := rec.status

		m.RequestsTotal.WithLabelValues(method, route, http.StatusText(status)).Inc()
		m.RequestDuration.WithLabelValues(method, route).Observe(duration)

		log := WithTrace(r.Context(), logger)
		log.Log(r.Context(), accessLevel(status), "http_request",
			"request_id", RequestIDFromContext(r.Context()),
			"method", method,
			"route", route,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", duration*1000,
			"remote_addr", r.RemoteAddr,
		)
	})
}

// accessLevel возвращает ERROR для клиентских/серверных ошибок (>=400) и
// INFO для успешных запросов. Это позволяет панели "HTTP failures + корневая
// причина" (и поиску по логам) находить ошибки по полю level, а не по статусу.
func accessLevel(status int) slog.Level {
	if status >= 400 {
		return slog.LevelError
	}
	return slog.LevelInfo
}
