// Recommendations Service — намеренно "хрупкий" вспомогательный сервис.
//
// Он отдаёт для товара список "рекомендованных" других товаров и
// popularity_score. Для приложения важнее
// не то, ЧТО он возвращает, а то, что его можно живьём заставить тормозить
// или отдавать ошибки через /admin/chaos — и посмотреть, как backend (с
// circuit breaker'ом) и вся картина в Grafana реагируют на это.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"sre-lab/recommendations/internal/obs"
)

// ── Динамически настраиваемый "хаос" ─────────────────────────────────────

type chaosConfig struct {
	mu        sync.RWMutex
	failRate  float64 // 0.0..1.0 — доля запросов, которые нужно завалить с 500
	latencyMs int     // добавочная задержка перед ответом
}

// rateLimiter — token bucket без внешней зависимости. Один instance
// намеренно лимитирует весь сервис: в лабе это наглядный admission control
// зависимости, а не пользовательская квота.
type rateLimiter struct {
	mu     sync.Mutex
	rps    float64
	burst  int
	tokens float64
	last   time.Time
}

func (l *rateLimiter) configure(rps float64, burst int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rps, l.burst = rps, burst
	l.tokens = float64(burst)
	l.last = time.Now()
}

func (l *rateLimiter) config() (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rps, l.burst
}

// allow returns a whole number suitable for Retry-After.
func (l *rateLimiter) allow() (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rps <= 0 {
		return true, 0
	}
	now := time.Now()
	l.tokens = math.Min(float64(l.burst), l.tokens+now.Sub(l.last).Seconds()*l.rps)
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	return false, int(math.Ceil((1 - l.tokens) / l.rps))
}

func (c *chaosConfig) get() (float64, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failRate, c.latencyMs
}

func (c *chaosConfig) set(failRate float64, latencyMs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failRate = failRate
	c.latencyMs = latencyMs
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── Данные ────────────────────────────────────────────────────────────

type recommendation struct {
	ProductID int     `json:"product_id"`
	Score     float64 `json:"score"`
}

type response struct {
	ProductID       int              `json:"product_id"`
	PopularityScore float64          `json:"popularity_score"`
	Recommended     []recommendation `json:"recommended"`
}

// makeRecommendations — детерминированная "рекомендация" без базы данных:
// сервис специально максимально простой и независимый, чтобы его было
// легко довести до отказа.
func makeRecommendations(productID int) response {
	src := rand.New(rand.NewSource(int64(productID) * 2654435761))
	total := 12
	recs := make([]recommendation, 0, 3)
	seen := map[int]bool{productID: true}
	for len(recs) < 3 {
		candidate := src.Intn(total) + 1
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		recs = append(recs, recommendation{
			ProductID: candidate,
			Score:     float64(src.Intn(50)+50) / 100.0,
		})
	}
	return response{
		ProductID:       productID,
		PopularityScore: float64(src.Intn(500)) / 100.0,
		Recommended:     recs,
	}
}

func main() {
	serviceName := envOr("SERVICE_NAME", "recommendations")
	addr := envOr("HTTP_ADDR", ":8082")
	otlpEndpoint := envOr("OTLP_ENDPOINT", "tempo:4318")

	logger := obs.NewLogger(serviceName)
	metrics := obs.NewMetrics("recommendations")
	tracer := otel.Tracer(serviceName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := obs.InitTracing(ctx, serviceName, otlpEndpoint)
	if err != nil {
		logger.Error("failed to init tracing", "error", err)
		os.Exit(1)
	}
	defer shutdownTracing(context.Background())

	chaos := &chaosConfig{}
	chaos.set(envFloat("CHAOS_FAIL_RATE", 0), envInt("CHAOS_LATENCY_MS", 0))
	limiter := &rateLimiter{}
	initialLimit := envFloat("RATE_LIMIT_RPS", 0)
	limiter.configure(initialLimit, envInt("RATE_LIMIT_BURST", 1))
	metrics.RateLimitConfiguredR.Set(initialLimit)

	mux := http.NewServeMux()

	mux.Handle("GET /recommendations/{id}", obs.Instrument(metrics, logger, "GET", "/recommendations/:id",
		func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tracer.Start(r.Context(), "recommendations.get")
			defer span.End()

			idStr := r.PathValue("id")
			id, err := strconv.Atoi(idStr)
			if err != nil {
				http.Error(w, "invalid product id", http.StatusBadRequest)
				return
			}
			span.SetAttributes(attribute.Int("product.id", id))
			if allowed, retryAfter := limiter.allow(); !allowed {
				seconds := max(1, retryAfter)
				span.SetStatus(codes.Error, "rate limited")
				span.SetAttributes(attribute.Int("http.response.status_code", http.StatusTooManyRequests))
				metrics.RateLimitRejections.Inc()
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				obs.WithTrace(ctx, logger).Warn("rate_limit_rejected",
					"request_id", obs.RequestIDFromContext(ctx), "product_id", id, "retry_after_seconds", seconds)
				http.Error(w, "recommendations rate limited", http.StatusTooManyRequests)
				return
			}

			failRate, latencyMs := chaos.get()

			if latencyMs > 0 {
				_, delaySpan := tracer.Start(ctx, "chaos.artificial_latency")
				delaySpan.SetAttributes(attribute.Int("chaos.latency_ms", latencyMs))
				time.Sleep(time.Duration(latencyMs) * time.Millisecond)
				delaySpan.End()
			}

			if failRate > 0 && rand.Float64() < failRate {
				span.SetStatus(codes.Error, "chaos: injected failure")
				log := obs.WithTrace(ctx, logger)
				log.Warn("chaos_injected_failure",
					"request_id", obs.RequestIDFromContext(ctx),
					"product_id", id,
					"fail_rate", failRate,
				)
				http.Error(w, "recommendations temporarily unavailable", http.StatusServiceUnavailable)
				return
			}

			resp := makeRecommendations(id)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// ── Управление хаосом "на лету" — без пересборки/рестарта контейнера,
	// чтобы можно было наблюдать деградацию сервиса.
	mux.HandleFunc("GET /admin/chaos", func(w http.ResponseWriter, r *http.Request) {
		fr, lat := chaos.get()
		rps, burst := limiter.config()
		json.NewEncoder(w).Encode(map[string]any{"fail_rate": fr, "latency_ms": lat, "rate_limit_rps": rps, "rate_limit_burst": burst})
	})
	mux.HandleFunc("POST /admin/chaos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FailRate       *float64 `json:"fail_rate"`
			LatencyMs      *int     `json:"latency_ms"`
			RateLimitRPS   *float64 `json:"rate_limit_rps"`
			RateLimitBurst *int     `json:"rate_limit_burst"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		fr, lat := chaos.get()
		if body.FailRate != nil {
			fr = *body.FailRate
		}
		if body.LatencyMs != nil {
			lat = *body.LatencyMs
		}
		if fr < 0 || fr > 1 || lat < 0 {
			http.Error(w, "fail_rate must be 0..1 and latency_ms must be >= 0", http.StatusBadRequest)
			return
		}
		chaos.set(fr, lat)
		rps, burst := limiter.config()
		if body.RateLimitRPS != nil {
			rps = *body.RateLimitRPS
		}
		if body.RateLimitBurst != nil {
			burst = *body.RateLimitBurst
		}
		if rps < 0 || burst < 1 {
			http.Error(w, "rate_limit_rps must be >= 0 and rate_limit_burst must be >= 1", http.StatusBadRequest)
			return
		}
		limiter.configure(rps, burst)
		metrics.RateLimitConfiguredR.Set(rps)
		logger.Warn("chaos_config_changed", "fail_rate", fr, "latency_ms", lat, "rate_limit_rps", rps, "rate_limit_burst", burst)
		json.NewEncoder(w).Encode(map[string]any{"fail_rate": fr, "latency_ms": lat, "rate_limit_rps": rps, "rate_limit_burst": burst})
	})

	mux.Handle("GET /metrics", promhttp.Handler())
	handler := otelhttp.NewHandler(
		obs.WithRequestID(mux),
		serviceName+"-server",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/metrics"
		}),
	)

	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		logger.Info("recommendations service started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	fmt.Println("shutting down recommendations service...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
