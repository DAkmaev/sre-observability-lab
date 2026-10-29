// Backend API — витрина товаров. Основной сервис приложения:
// на нём видно все три столпа наблюдаемости разом, а вызов recommendations
// через circuit breaker — это живая иллюстрация graceful degradation из
// отказоустойчивости.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"sre-lab/backend/internal/breaker"
	"sre-lab/backend/internal/obs"
	"sre-lab/backend/internal/payment"
	"sre-lab/backend/internal/resilience"
	"sre-lab/backend/internal/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// outcomeAttrs превращает outcome механизмов отказоустойчивости в OTel-атрибуты
// спана, чтобы в Tempo было видно, какие паттерны сработали для этой зависимости.
func outcomeAttrs(o *resilience.Outcome) []attribute.KeyValue {
	if o == nil || !o.Any() {
		return nil
	}
	return []attribute.KeyValue{
		attribute.Bool("resilience.retried", o.Retried),
		attribute.Int("resilience.retries", o.Retries),
		attribute.Bool("resilience.bulkhead_rejected", o.BulkheadRej),
		attribute.Bool("resilience.fallback_hit", o.FallbackHit),
		attribute.Bool("resilience.timed_out", o.TimedOut),
		attribute.Int64("resilience.total_ms", o.TotalTime.Milliseconds()),
	}
}

type productResponse struct {
	store.Product
	RecommendationsAvailable bool                     `json:"recommendations_available"`
	PopularityScore          float64                  `json:"popularity_score,omitempty"`
	Recommended              []breaker.Recommendation `json:"recommended,omitempty"`
}

// recommendationsPolicy переключает поведение витрины прямо во время
// занятия. В strict зависимость считается обязательной; в graceful она
// optional и карточка товара остаётся доступной без рекомендаций.
type recommendationsPolicy struct {
	mu   sync.RWMutex
	mode string
}

func newRecommendationsPolicy(mode string) *recommendationsPolicy {
	p := &recommendationsPolicy{mode: "graceful"}
	if mode == "strict" {
		p.mode = mode
	}
	return p
}

func (p *recommendationsPolicy) get() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

func (p *recommendationsPolicy) set(mode string) error {
	if mode != "strict" && mode != "graceful" {
		return fmt.Errorf("mode must be strict or graceful")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	return nil
}

// checkoutRequest — то, что фронтенд шлёт при нажатии "Оформить заказ":
// весь текущий состав корзины (id товара + количество). Корзина сама по
// себе на бэкенде нигде не хранится — она живёт в state React, здесь
// только результат её оформления (заказ).
type checkoutRequest struct {
	Items []store.CartItem `json:"items"`
}

type checkoutResponse struct {
	Order *store.Order `json:"order"`
}

// requireAdmin — ролевой гейт для админ-ручек (/admin/* за прокси-слоем
// /lab-admin/*). Без сессии → 401; с сессией, но роль не admin → 403
// (по правилам проекта: 401 = нет аутентификации, 403 = роль не подходит).
// Каждый отказ считаем метрикой AdminDenied и пишем лог.
func requireAdmin(logger *slog.Logger, metrics *obs.BusinessMetrics, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := obs.UserFromContext(r.Context())
		if !ok {
			obs.WithTrace(r.Context(), logger).Warn("admin_access_denied",
				"request_id", obs.RequestIDFromContext(r.Context()), "reason", "no_session")
			metrics.AdminDenied.Inc()
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if user.Role != "admin" {
			obs.WithTrace(r.Context(), logger).Warn("admin_access_denied",
				"request_id", obs.RequestIDFromContext(r.Context()),
				"user_id", user.ID, "user_name", user.Name, "user_role", user.Role, "reason", "not_admin")
			metrics.AdminDenied.Inc()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// reserveInventory резервирует склад по позициям корзины в том порядке, в
// котором они пришли. Если какого-то товара не хватает, возвращает
// ErrInsufficientStock, предварительно компенсировав (UnreserveStock) всё, что
// уже было зарезервировано в этой же попытке — чтобы не проедать остатки
// при сорвавшейся оплате. Технические ошибки БД пробрасываются как есть.
func reserveInventory(ctx context.Context, db store.Storage, items []store.PricedItem, requestID string) (reserved map[int]int, err error) {
	reserved = make(map[int]int, len(items))
	for _, it := range items {
		if it.Quantity <= 0 {
			continue
		}
		if err := db.ReserveStock(ctx, it.ProductID, it.Quantity, requestID); err != nil {
			if errors.Is(err, store.ErrInsufficientStock) {
				for pid, qty := range reserved {
					_ = db.UnreserveStock(ctx, pid, qty, requestID)
				}
				return nil, store.ErrInsufficientStock
			}
			return nil, err
		}
		reserved[it.ProductID] = it.Quantity
	}
	return reserved, nil
}

// releaseStock компенсирует резервирование после неудачной оплаты
// (reason="unreserve"). Возвращает число успешно возвращённых позиций и первую
// встреченную техническую ошибку (если такая была).
func releaseStock(ctx context.Context, db store.Storage, reserved map[int]int, requestID string) (released int, firstErr error) {
	for pid, qty := range reserved {
		if err := db.UnreserveStock(ctx, pid, qty, requestID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		released++
	}
	return released, firstErr
}

// validateReview проверяет бизнес-ограничения отзыва: рейтинг в диапазоне 1..5
// и непустой (после trim) комментарий. Ошибки здесь — это штатные 400, а не
// сбои БД.
func validateReview(rating int, comment string) error {
	if rating < 1 || rating > 5 {
		return fmt.Errorf("rating must be between 1 and 5")
	}
	if strings.TrimSpace(comment) == "" {
		return fmt.Errorf("comment must not be empty")
	}
	return nil
}

// orderFilter определяет видимость истории покупок: админ видит все заказы
// (filterUserID=0), обычный пользователь — только свои. Возвращается значение
// для передачи в store.Storage.ListOrders.
func orderFilter(userID int, role string) int {
	if role == "admin" {
		return 0
	}
	return userID
}

func main() {
	serviceName := envOr("SERVICE_NAME", "backend")
	addr := envOr("HTTP_ADDR", ":8081")
	otlpEndpoint := envOr("OTLP_ENDPOINT", "tempo:4318")
	databaseURL := envOr("DATABASE_URL", "postgres://sre:sre@postgres:5432/store?sslmode=disable")
	recommendationsURL := envOr("RECOMMENDATIONS_URL", "http://recommendations:8082")
	paymentURL := envOr("PAYMENT_URL", "http://payment:8083")

	logger := obs.NewLogger(serviceName)
	metrics := obs.NewMetrics("backend")
	bizMetrics := obs.NewBusinessMetrics()
	tracer := otel.Tracer(serviceName)

	// In-memory аутентификационные сессии (токен → user). При рестарте
	// backend сессии сбрасываются, а users/отзывы/склад остаются в БД.
	sessions := obs.SessionStore(nil)

	// Активные сессии считаются по активности посетителя, а не по входу:
	// каждый клиент получает анонимный cookie, и пока он присылает запросы,
	// его сессия живёт (TTL 10с). Именно этот gauge отображается на панели
	// «Активные сессии».
	visitors := obs.VisitorStore(func(n int) { bizMetrics.SetActiveSessions(n) })

	// Динамические настройки механизмов отказоустойчивости:
	// retry, timeout, bulkhead, fallback. Меняются на лету через
	// /admin/resilience, чтобы каждый паттерн можно было показать отдельно.
	resilienceCfg := resilience.DefaultConfig()
	resilienceMetrics := resilience.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Janitor периодически вычищает протухшие in-memory сессии посетителей,
	// чтобы gauge активных сессий падал, когда клиент больше не шлёт запросы.
	visitors.StartJanitor(ctx)
	// Auth-сессии тоже не должны накапливаться без очистки (TTL 15м).
	sessions.StartJanitor(ctx)

	shutdownTracing, err := obs.InitTracing(ctx, serviceName, otlpEndpoint)
	if err != nil {
		logger.Error("failed to init tracing", "error", err)
		os.Exit(1)
	}
	defer shutdownTracing(context.Background())

	dbPool, err := store.New(ctx, databaseURL)
	if err != nil {
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer dbPool.Close()

	// Хендлеры работают с хранилищем через интерфейс store.Storage, чтобы
	// бизнес-логику можно было тестировать на fake-репозитории без БД.
	var db store.Storage = dbPool

	recClient := breaker.NewRecommendationsClient(recommendationsURL, logger, resilienceCfg, resilienceMetrics)
	payClient := payment.NewClient(paymentURL, logger, resilienceCfg, resilienceMetrics)
	recPolicy := newRecommendationsPolicy(envOr("RECOMMENDATIONS_FAILURE_MODE", "graceful"))

	mux := http.NewServeMux()

	// GET /api/products — список товаров (витрина).
	mux.HandleFunc("/api/products", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, span := tracer.Start(r.Context(), "products.list")
		defer span.End()

		products, err := db.ListProducts(ctx)
		if err != nil {
			obs.WithTrace(ctx, logger).Error("failed to list products",
				"request_id", obs.RequestIDFromContext(ctx), "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(products)
	})

	// GET /api/products/{id} — карточка товара + рекомендации (best effort).
	mux.HandleFunc("/api/products/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()

		// Extract ID from path
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) < 4 {
			http.Error(w, "invalid product id", http.StatusBadRequest)
			return
		}
		id, err := strconv.Atoi(pathParts[3])
		if err != nil {
			http.Error(w, "invalid product id", http.StatusBadRequest)
			return
		}

		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		ctxProduct, spanProduct := tracer.Start(ctx, "products.get")
		spanProduct.SetAttributes(attribute.Int("product.id", id))
		p, err := db.GetProduct(ctxProduct, id)
		spanProduct.End()
		if err != nil {
			log.Warn("product not found", "request_id", requestID, "product_id", id, "error", err)
			http.Error(w, "product not found", http.StatusNotFound)
			return
		}

		resp := productResponse{Product: *p}

		// ── Вызов "хрупкой" зависимости через circuit breaker + учебные
		// механизмы retry/timeout/bulkhead/fallback ──────────
		// В strict сбой recommendations ломает карточку; в graceful товар
		// отдаётся без рекомендаций. Если включён fallback-кэш, отдаётся
		// последний успешный результат рекомендаций вместо молчаливого отказа.
		recCtx, recSpan := tracer.Start(ctx, "recommendations.fetch")
		rec, recOutcome, err := recClient.Get(recCtx, id)

		if out := outcomeAttrs(recOutcome); out != nil {
			recSpan.SetAttributes(out...)
		}

		if err != nil {
			recSpan.SetStatus(codes.Error, err.Error())
			recSpan.End()
			resp.RecommendationsAvailable = false
			mode := recPolicy.get()
			log.Warn("recommendations degraded",
				"request_id", requestID,
				"product_id", id,
				"mode", mode,
				"circuit_breaker_state", recClient.State(),
				"fallback_hit", recOutcome != nil && recOutcome.FallbackHit,
				"retried", recOutcome != nil && recOutcome.Retried,
				"timed_out", recOutcome != nil && recOutcome.TimedOut,
				"bulkhead_rejected", recOutcome != nil && recOutcome.BulkheadRej,
				"error", err.Error(),
			)
			if mode == "strict" {
				status := http.StatusServiceUnavailable
				if breaker.IsRateLimited(err) {
					status = http.StatusTooManyRequests
					w.Header().Set("Retry-After", "1")
				}
				http.Error(w, "recommendations temporarily unavailable", status)
				return
			}
		} else {
			recSpan.End()
			resp.RecommendationsAvailable = true
			resp.PopularityScore = rec.PopularityScore
			resp.Recommended = rec.Recommended
			if recOutcome != nil && recOutcome.FallbackHit {
				log.Warn("recommendations fallback served",
					"request_id", requestID,
					"product_id", id,
					"circuit_breaker_state", recClient.State(),
					"fallback_ttl_ms", recOutcome.FallbackTTL.Milliseconds(),
				)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// Управление режимом деградации. Ручка доступна на опубликованном
	// backend:8081, поэтому изменения применяются без перезапуска сервиса.
	mux.Handle("GET /admin/recommendations-policy", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"mode": recPolicy.get()})
	}))
	mux.Handle("POST /admin/recommendations-policy", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := recPolicy.set(body.Mode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Warn("recommendations_policy_changed", "mode", body.Mode)
		json.NewEncoder(w).Encode(map[string]string{"mode": body.Mode})
	}))

	// Управление учебными паттернами отказоустойчивости (retry, timeout,
	// bulkhead и fallback. Каждый механизм включается отдельно и
	// настраивается во время работы сервиса.
	mux.Handle("GET /admin/resilience", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resilienceCfg.Snapshot())
	}))
	mux.Handle("POST /admin/resilience", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		var patch resilience.Patch
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		resilienceCfg.Set(patch)
		logger.Warn("resilience_config_changed", "snapshot", resilienceCfg.Snapshot())
		json.NewEncoder(w).Encode(resilienceCfg.Snapshot())
	}))

	// POST /admin/products/{id}/restock — восполнение остатка, admin-only.
	// Открывается наружу через gateway как /lab-admin/products/{id}/restock.
	mux.Handle("POST /admin/products/{id}/restock", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "inventory.restock")
		defer span.End()
		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid product id", http.StatusBadRequest)
			return
		}
		var body struct {
			Qty int `json:"qty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Qty <= 0 {
			http.Error(w, "qty must be positive", http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.Int("product.id", id), attribute.Int("qty", body.Qty))

		if err := db.Restock(ctx, id, body.Qty, requestID); err != nil {
			log.Error("inventory_restock_failed", "request_id", requestID, "product_id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		stock, err := db.GetStock(ctx, id)
		if err != nil {
			log.Error("inventory_restock_failed", "request_id", requestID, "product_id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		bizMetrics.InventoryRestock.Inc()
		bizMetrics.StockMovement.WithLabelValues("restock").Inc()
		log.Info("inventory_restocked", "request_id", requestID, "product_id", id, "qty", body.Qty, "stock", stock)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"product_id": id, "stock": stock})
	}))

	// GET /admin/stock — текущие остатки для Inventory-карточки админки.
	mux.Handle("GET /admin/stock", requireAdmin(logger, bizMetrics, func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "inventory.list")
		defer span.End()

		stock, err := db.ListStock(ctx)
		if err != nil {
			obs.WithTrace(ctx, logger).Error("failed to list stock",
				"request_id", obs.RequestIDFromContext(ctx), "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if stock == nil {
			stock = []store.StockEntry{}
		}
		// Обновляем gauge низкого остатка по числу товаров с нулевым остатком.
		low := 0
		for _, e := range stock {
			if !e.InStock {
				low++
			}
		}
		bizMetrics.LowStockProducts.Set(float64(low))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stock)
	}))

	// POST /api/orders — оформление корзины ("Купить"). Основной путь для
	// работы с payment: products.list/get — это чтение, а вот
	// checkout — это тот самый "денежный" путь, который интереснее всего
	// ломать и наблюдать за деградацией.
	mux.HandleFunc("/api/orders", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, span := tracer.Start(r.Context(), "checkout.process")
		defer span.End()

		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		// Заказ всегда создаётся под конкретным покупателем — без входа
		// оформить покупку нельзя (иначе "история = свои покупки" и
		// "кто купил" для админа теряют смысл).
		user, ok := obs.UserFromContext(ctx)
		if !ok {
			log.Warn("checkout_failed",
				"request_id", requestID, "reason", "unauthenticated")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.Int("user.id", user.ID))

		var req checkoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Items) == 0 {
			http.Error(w, "empty or invalid cart", http.StatusBadRequest)
			return
		}

		// ── Считаем цену по данным из БД, не доверяя фронтенду ───────
		priceCtx, priceSpan := tracer.Start(ctx, "orders.price_items")
		pricedItems, totalCents, err := db.PriceCartItems(priceCtx, req.Items)
		priceSpan.End()
		if err != nil {
			log.Warn("checkout failed: unknown product in cart",
				"request_id", requestID, "error", err)
			http.Error(w, "one of the products no longer exists", http.StatusBadRequest)
			return
		}
		if totalCents <= 0 {
			http.Error(w, "empty cart", http.StatusBadRequest)
			return
		}
		span.SetAttributes(
			attribute.Int("order.total_cents", totalCents),
			attribute.Int("order.item_count", len(pricedItems)),
		)

		// ── Резервируем склад до обращения к оплате ─────────────────────
		// Списываем остатки по каждому товару корзины. Если товара не хватает —
		// заказ НЕ создаётся (в отличие от payment-флоу, где заказ сохраняется
		// всегда): отвечаем 409 с отдельной метрикой.
		reserveCtx, reserveSpan := tracer.Start(ctx, "inventory.reserve")
		reserved, err := reserveInventory(reserveCtx, db, pricedItems, requestID)
		reserveSpan.End()
		if err != nil {
			if errors.Is(err, store.ErrInsufficientStock) {
				bizMetrics.OrdersBlockedByStock.Inc()
				reserveSpan.SetStatus(codes.Error, "insufficient stock")
				log.Warn("order_blocked_low_stock",
					"request_id", requestID)
				http.Error(w, "insufficient stock", http.StatusConflict)
				return
			}
			reserveSpan.SetStatus(codes.Error, err.Error())
			log.Error("inventory.reserve failed", "request_id", requestID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for range reserved {
			bizMetrics.StockMovement.WithLabelValues("reserve").Inc()
		}

		// ── Оплата через payment-сервис (с circuit breaker и механизмы отказоустойчивости) ──
		payCtx, paySpan := tracer.Start(ctx, "payment.request")
		orderRef := fmt.Sprintf("req-%s-%d", requestID, time.Now().UnixNano())
		chargeResp, payOutcome, payErr := payClient.Charge(payCtx, orderRef, totalCents)
		if out := outcomeAttrs(payOutcome); out != nil {
			paySpan.SetAttributes(out...)
		}
		paySpan.End()

		status, paymentID, failureReason := "paid", "", ""
		if chargeResp != nil {
			paymentID = chargeResp.PaymentID
		}
		if payErr != nil {
			status = "failed"
			// Оплата не прошла — возвращаем ранее зарезервированные остатки
			// на склад (компенсирующее действие). Пишем stock_movement с
			// reason="unreserve" — это дополнительный источник наблюдаемости.
			released, unreserveErr := releaseStock(ctx, db, reserved, requestID)
			for i := 0; i < released; i++ {
				bizMetrics.StockMovement.WithLabelValues("unreserve").Inc()
			}
			if unreserveErr != nil {
				log.Error("unreserve_stock_failed", "request_id", requestID, "error", unreserveErr)
			}
			var decline *payment.BusinessDecline
			if errors.As(payErr, &decline) {
				// Штатный отказ карты — не деградация сервиса.
				failureReason = decline.Reason
				log.Warn("checkout failed: card declined",
					"request_id", requestID,
					"payment_id", paymentID,
					"reason", failureReason,
					"total_cents", totalCents,
				)
			} else {
				// Техническая проблема с payment-сервисом (таймаут,
				// 5xx, разомкнутая цепь) — то, что ищут студенты в
				// лабе по трейсам/логам/метрикам.
				failureReason = "payment_service_unavailable"
				log.Error("checkout failed: payment service degraded",
					"request_id", requestID,
					"circuit_breaker_state", payClient.State(),
					"retried", payOutcome != nil && payOutcome.Retried,
					"timed_out", payOutcome != nil && payOutcome.TimedOut,
					"bulkhead_rejected", payOutcome != nil && payOutcome.BulkheadRej,
					"fallback_hit", payOutcome != nil && payOutcome.FallbackHit,
					"total_cents", totalCents,
					"error", payErr.Error(),
				)
			}
			span.SetStatus(codes.Error, "checkout: "+failureReason)
		} else {
			log.Info("checkout succeeded",
				"request_id", requestID,
				"payment_id", paymentID,
				"total_cents", totalCents,
				"item_count", len(pricedItems),
			)
		}

		// ── Заказ сохраняется в любом случае — и успешный, и неудачный,
		// чтобы в БД была полная история попыток оформления заказа.
		saveCtx, saveSpan := tracer.Start(ctx, "orders.save")
		order, err := db.CreateOrder(saveCtx, user.ID, pricedItems, totalCents, status, paymentID, failureReason, requestID)
		saveSpan.End()
		if err != nil {
			log.Error("failed to save order", "request_id", requestID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if status == "failed" {
			w.WriteHeader(http.StatusPaymentRequired) // 402 — заказ создан, но оплата не прошла
		}
		json.NewEncoder(w).Encode(checkoutResponse{Order: order})
	})

	// GET /api/orders — история заказов. Требует входа: admin видит все
	// покупки, обычный пользователь — только свои.
	mux.HandleFunc("GET /api/orders", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "orders.list")
		defer span.End()

		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		user, ok := obs.UserFromContext(ctx)
		if !ok {
			log.Warn("orders.list denied",
				"request_id", requestID, "reason", "unauthenticated")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.Int("user.id", user.ID))

		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		// Админ смотрит всю историю; обычный пользователь — только свои заказы.
		filterUserID := orderFilter(user.ID, user.Role)

		orders, err := db.ListOrders(ctx, filterUserID, limit)
		if err != nil {
			log.Error("failed to list orders", "request_id", requestID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if orders == nil {
			orders = []store.Order{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(orders)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("db unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok, recommendations_cb=" + recClient.State() + ", payment_cb=" + payClient.State()))
	})

	// ── Пользователи и сессии ─────────────────────────────────────────────
	// Лёгкая аутентификация для лабы: вход по имени без пароля. Токен сессии
	// живёт in-memory в backend; фронт хранит его в localStorage и шлёт в
	// заголовке X-Session-Token вместе с X-User-Name.

	// GET /api/session — текущий пользователь из заголовков (или anonymous).
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "session.get")
		defer span.End()

		token := r.Header.Get("X-Session-Token")
		u, ok := obs.UserFromContext(r.Context())
		valid := ok && token != ""
		resp := map[string]interface{}{
			"name":   "anonymous",
			"role":   "guest",
			"logged": false,
		}
		if valid {
			resp["name"] = u.Name
			resp["role"] = u.Role
			resp["logged"] = true
			span.SetAttributes(attribute.Int("user.id", u.ID))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// POST /api/session {name} — вход: upsert юзера, создать in-memory сессию.
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "session.create")
		defer span.End()

		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}

		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		user, err := db.UpsertUserByName(ctx, body.Name)
		if err != nil {
			log.Error("failed to upsert user", "request_id", requestID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		token := sessions.Create(obs.User{ID: user.ID, Name: user.Name, Role: user.Role})
		bizMetrics.UserLogins.Inc()
		bizMetrics.UserActions.WithLabelValues("login").Inc()
		span.SetAttributes(attribute.Int("user.id", user.ID))
		log.Info("user_login", "request_id", requestID, "user_id", user.ID, "user_name", user.Name, "user_role", user.Role)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token": token, "name": user.Name, "role": user.Role,
		})
	})

	// DELETE /api/session — выход: удалить сессию.
	mux.HandleFunc("DELETE /api/session", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "session.delete")
		defer span.End()

		token := r.Header.Get("X-Session-Token")
		u, _ := obs.UserFromContext(r.Context())
		if token != "" {
			sessions.Delete(token)
			bizMetrics.UserLogouts.Inc()
			bizMetrics.UserActions.WithLabelValues("logout").Inc()
			span.SetAttributes(attribute.Int("user.id", u.ID))
			obs.WithTrace(ctx, logger).Info("user_logout",
				"request_id", obs.RequestIDFromContext(ctx), "user_id", u.ID, "user_name", u.Name)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ── Отзывы и рейтинги ─────────────────────────────────────────────────
	// GET /api/products/{id}/reviews — список отзывов, доступно всем.
	mux.HandleFunc("GET /api/products/{id}/reviews", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "reviews.list")
		defer span.End()
		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid product id", http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.Int("product.id", id))

		reviews, err := db.ListReviews(ctx, id)
		if err != nil {
			log.Error("failed to list reviews", "request_id", requestID, "product_id", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if reviews == nil {
			reviews = []store.Review{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reviews)
	})

	// POST /api/reviews {product_id, rating, comment} — требует авторизации.
	mux.HandleFunc("POST /api/reviews", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "reviews.post")
		defer span.End()
		log := obs.WithTrace(ctx, logger)
		requestID := obs.RequestIDFromContext(ctx)

		user, ok := obs.UserFromContext(ctx)
		if !ok {
			log.Warn("review_failed", "request_id", requestID, "reason", "unauthenticated")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.Int("user.id", user.ID))

		var body struct {
			ProductID int    `json:"product_id"`
			Rating    int    `json:"rating"`
			Comment   string `json:"comment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			bizMetrics.ReviewRejected.Inc()
			log.Warn("review_failed", "request_id", requestID, "reason", "bad_json", "user_id", user.ID)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := validateReview(body.Rating, body.Comment); err != nil {
			bizMetrics.ReviewRejected.Inc()
			log.Warn("review_failed", "request_id", requestID, "reason", err.Error(), "rating", body.Rating, "user_id", user.ID)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.Int("product.id", body.ProductID), attribute.Int("rating", body.Rating))

		review, err := db.CreateReview(ctx, body.ProductID, user.ID, body.Rating, body.Comment)
		if err != nil {
			log.Error("review_failed", "request_id", requestID, "reason", "db", "product_id", body.ProductID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		bizMetrics.ReviewPosted.Inc()
		bizMetrics.ReviewRating.WithLabelValues(strconv.Itoa(body.Rating)).Inc()
		log.Info("review_created", "request_id", requestID, "review_id", review.ID,
			"product_id", body.ProductID, "user_id", user.ID, "rating", body.Rating)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(review)
	})

	mux.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)

	handler := otelhttp.NewHandler(
		obs.WithRequestID(
			obs.InstrumentServer(metrics, logger,
				obs.TrackVisitors(visitors)(
					obs.UserContext(sessions)(mux),
				),
			),
		),
		serviceName+"-server",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/metrics"
		}),
	)

	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		logger.Info("backend service started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down backend service...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
