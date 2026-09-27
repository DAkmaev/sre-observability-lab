// Payment Service — обрабатывает оплату заказа в два шага, каждый со своим
// span'ом:
//
//  1. bank.authorize   — "звонок" в банк-эмитент карты (симулируется, без
//     реального сетевого вызова, но с реалистичной задержкой).
//  2. payments.persist — запись результата платежа в Postgres.
//
// Оба шага можно независимо ломать через /admin/chaos (stage=bank|db,
// fail_rate, latency_ms, fail_kind) — это и есть материал для лабы
// "найди проблему по трейсам/логам/метрикам", см. PAYMENT_INCIDENT_LAB.md
// в корне репозитория.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"sre-lab/payment/internal/obs"
)

// ── Динамически настраиваемый "хаос" ─────────────────────────────────────

type chaosConfig struct {
	mu        sync.RWMutex
	failRate  float64 // 0.0..1.0 — доля запросов, которые нужно завалить
	latencyMs int     // добавочная задержка перед ответом
	stage     string  // "bank" (по умолчанию) или "db" — где именно инжектим задержку/отказ
	failKind  string  // причина отказа: card_declined | issuer_timeout | issuer_unavailable | db_unavailable
}

type chaosSnapshot struct {
	FailRate  float64 `json:"fail_rate"`
	LatencyMs int     `json:"latency_ms"`
	Stage     string  `json:"stage"`
	FailKind  string  `json:"fail_kind"`
}

func (c *chaosConfig) get() chaosSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return chaosSnapshot{c.failRate, c.latencyMs, c.stage, c.failKind}
}

func (c *chaosConfig) set(s chaosSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failRate = s.FailRate
	c.latencyMs = s.LatencyMs
	c.stage = s.Stage
	c.failKind = s.FailKind
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

type chargeRequest struct {
	OrderRef    string `json:"order_ref"`
	AmountCents int    `json:"amount_cents"`
}

type chargeResponse struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"` // approved | declined | error
	Reason    string `json:"reason,omitempty"`
}

func newPaymentID() string {
	return fmt.Sprintf("pay_%08x", rand.Uint32())
}

// reasonToHTTPStatus решает, это "бизнес-отказ" (карта отклонена банком —
// нормальная ситуация, HTTP 200 с status=declined) или "техническая
// ошибка" зависимости (нужно вернуть код ошибки, чтобы backend понял,
// что стоит деградировать/повторить, а не просто показать "карта неверна").
func reasonToHTTPStatus(reason string) (status int, businessDecline bool) {
	switch reason {
	case "card_declined":
		return http.StatusOK, true
	case "issuer_timeout":
		return http.StatusGatewayTimeout, false
	case "issuer_unavailable":
		return http.StatusBadGateway, false
	case "db_unavailable":
		return http.StatusInternalServerError, false
	default:
		return http.StatusInternalServerError, false
	}
}

func main() {
	serviceName := envOr("SERVICE_NAME", "payment")
	addr := envOr("HTTP_ADDR", ":8083")
	otlpEndpoint := envOr("OTLP_ENDPOINT", "tempo:4318")
	databaseURL := envOr("DATABASE_URL", "postgres://sre:sre@postgres:5432/store?sslmode=disable")

	logger := obs.NewLogger(serviceName)
	metrics := obs.NewMetrics("payment")
	tracer := otel.Tracer(serviceName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := obs.InitTracing(ctx, serviceName, otlpEndpoint)
	if err != nil {
		logger.Error("failed to init tracing", "error", err)
		os.Exit(1)
	}
	defer shutdownTracing(context.Background())

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	chaos := &chaosConfig{}
	chaos.set(chaosSnapshot{
		FailRate:  envFloat("CHAOS_FAIL_RATE", 0),
		LatencyMs: envInt("CHAOS_LATENCY_MS", 0),
		Stage:     envOr("CHAOS_STAGE", "bank"),
		FailKind:  envOr("CHAOS_FAIL_KIND", "card_declined"),
	})

	mux := http.NewServeMux()

	mux.Handle("POST /payments/charge", obs.Instrument(metrics, logger, "POST", "/payments/charge",
		func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tracer.Start(r.Context(), "payment.charge")
			defer span.End()

			log := obs.WithTrace(ctx, logger)
			requestID := obs.RequestIDFromContext(ctx)

			var req chargeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			span.SetAttributes(
				attribute.String("payment.order_ref", req.OrderRef),
				attribute.Int("payment.amount_cents", req.AmountCents),
			)

			cs := chaos.get()
			paymentID := newPaymentID()

			// ── Шаг 1: bank.authorize ("звонок" в банк-эмитент) ──────────
			bankCtx, bankSpan := tracer.Start(ctx, "bank.authorize")
			bankSpan.SetAttributes(attribute.String("payment.order_ref", req.OrderRef))

			status, reason := "approved", "ok"
			if cs.Stage == "bank" && cs.LatencyMs > 0 {
				bankSpan.SetAttributes(attribute.Int("chaos.latency_ms", cs.LatencyMs))
				time.Sleep(time.Duration(cs.LatencyMs) * time.Millisecond)
			}
			if cs.Stage == "bank" && cs.FailRate > 0 && rand.Float64() < cs.FailRate {
				reason = cs.FailKind
				if reason == "" {
					reason = "card_declined"
				}
				status = "declined"
				if _, business := reasonToHTTPStatus(reason); !business {
					status = "error"
				}
				bankSpan.SetStatus(codes.Error, "chaos: "+reason)
			}
			bankSpan.End()

			if status != "approved" {
				httpStatus, business := reasonToHTTPStatus(reason)
				log.Warn("payment_bank_rejected",
					"request_id", requestID,
					"payment_id", paymentID,
					"order_ref", req.OrderRef,
					"status", status,
					"reason", reason,
					"business_decline", business,
				)
				span.SetStatus(codes.Error, "bank: "+reason)
				if _, err := pool.Exec(ctx,
					`INSERT INTO payments (id, order_ref, amount_cents, status, reason) VALUES ($1, $2, $3, $4, $5)`,
					paymentID, req.OrderRef, req.AmountCents, status, reason); err != nil {
					log.Error("payment_persist_failed_after_decline",
						"request_id", requestID, "payment_id", paymentID, "error", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(httpStatus)
				json.NewEncoder(w).Encode(chargeResponse{PaymentID: paymentID, Status: status, Reason: reason})
				return
			}

			// ── Шаг 2: payments.persist (запись результата в Postgres) ───
			persistCtx, persistSpan := tracer.Start(bankCtx, "payments.persist")
			if cs.Stage == "db" && cs.LatencyMs > 0 {
				persistSpan.SetAttributes(attribute.Int("chaos.latency_ms", cs.LatencyMs))
				time.Sleep(time.Duration(cs.LatencyMs) * time.Millisecond)
			}
			if cs.Stage == "db" && cs.FailRate > 0 && rand.Float64() < cs.FailRate {
				persistSpan.SetStatus(codes.Error, "chaos: db_unavailable")
				persistSpan.End()
				log.Error("payment_persist_failed",
					"request_id", requestID,
					"payment_id", paymentID,
					"order_ref", req.OrderRef,
					"error", "db_unavailable (chaos)",
				)
				span.SetStatus(codes.Error, "persist: db_unavailable")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(chargeResponse{PaymentID: paymentID, Status: "error", Reason: "db_unavailable"})
				return
			}

			_, err := pool.Exec(persistCtx,
				`INSERT INTO payments (id, order_ref, amount_cents, status, reason) VALUES ($1, $2, $3, $4, $5)`,
				paymentID, req.OrderRef, req.AmountCents, "approved", "ok")
			persistSpan.End()
			if err != nil {
				log.Error("payment_persist_failed",
					"request_id", requestID, "payment_id", paymentID, "error", err)
				span.SetStatus(codes.Error, "persist: "+err.Error())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(chargeResponse{PaymentID: paymentID, Status: "error", Reason: "db_unavailable"})
				return
			}

			log.Info("payment_approved",
				"request_id", requestID, "payment_id", paymentID,
				"order_ref", req.OrderRef, "amount_cents", req.AmountCents)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(chargeResponse{PaymentID: paymentID, Status: "approved"})
		}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("db unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// ── Управление хаосом "на лету" ──────────────────────────────────────
	mux.HandleFunc("GET /admin/chaos", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chaos.get())
	})
	mux.HandleFunc("POST /admin/chaos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FailRate  *float64 `json:"fail_rate"`
			LatencyMs *int     `json:"latency_ms"`
			Stage     *string  `json:"stage"`
			FailKind  *string  `json:"fail_kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		cs := chaos.get()
		if body.FailRate != nil {
			cs.FailRate = *body.FailRate
		}
		if body.LatencyMs != nil {
			cs.LatencyMs = *body.LatencyMs
		}
		if body.Stage != nil {
			cs.Stage = *body.Stage
		}
		if body.FailKind != nil {
			cs.FailKind = *body.FailKind
		}
		chaos.set(cs)
		logger.Warn("chaos_config_changed",
			"fail_rate", cs.FailRate, "latency_ms", cs.LatencyMs,
			"stage", cs.Stage, "fail_kind", cs.FailKind)
		json.NewEncoder(w).Encode(cs)
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
		logger.Info("payment service started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down payment service...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
