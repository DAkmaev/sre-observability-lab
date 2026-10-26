// Package obs собирает метрики, логи и трейсы в одном месте:
// трейсинг (OpenTelemetry → Tempo), структурированные логи (slog JSON →
// stdout, откуда их забирает Vector) и RED-метрики (Prometheus →
// VictoriaMetrics).
package obs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
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

		m.RequestsTotal.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
		m.RequestDuration.WithLabelValues(method, route).Observe(duration)

		log := WithTrace(r.Context(), logger)
		level := accessLevel(status)
		log.Log(r.Context(), level,
			fmt.Sprintf("%s %s -> %d %s", method, route, status, http.StatusText(status)),
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

// InstrumentServer собирает RED для backend, где маршруты объявлены в
// net/http ServeMux. Нормализация :id не даёт product ID раздувать
// cardinality метрик.
func InstrumentServer(m *Metrics, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.URL.Path
		// Более специфичный маршрут (под-ресурс товара) проверяем ДО общего
		// префикса /api/products/, иначе он был бы ошибочно записан как
		// /api/products/:id и загрязнил бы RED-метрики карточки товара.
		if strings.HasPrefix(route, "/api/products/") && strings.HasSuffix(route, "/reviews") {
			route = "/api/products/:id/reviews"
		} else if strings.HasPrefix(route, "/api/products/") {
			route = "/api/products/:id"
		}
		m.RequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		m.RequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())

		log := WithTrace(r.Context(), logger).With(
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method, "route", route, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Seconds()*1000,
			"remote_addr", r.RemoteAddr,
		)
		// Обогащаем access-лог identity запроса, если пользователь есть.
		if u, ok := UserFromContext(r.Context()); ok {
			log = log.With("user_id", u.ID, "user_role", u.Role)
		}
		log.Log(r.Context(), accessLevel(rec.status),
			fmt.Sprintf("%s %s -> %d %s", r.Method, route, rec.status, http.StatusText(rec.status)))
	})
}
