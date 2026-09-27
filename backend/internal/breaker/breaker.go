// Package breaker — HTTP-клиент до recommendations-сервиса, обёрнутый в
// circuit breaker. Это то самое
// "не добивать зависимость потоком запросов, если она уже отказала".
//
// Поверх circuit breaker навиваются и остальные механизмы отказоустойчивости: bulkhead (изоляция ресурсов), retry (повтор временных сбоев),
// явный timeout и fallback (кэш последнего успеха). Все они управляются
// на лету через /admin/resilience.
package breaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"sre-lab/backend/internal/resilience"
)

type Recommendation struct {
	ProductID int     `json:"product_id"`
	Score     float64 `json:"score"`
}

type Response struct {
	ProductID       int              `json:"product_id"`
	PopularityScore float64          `json:"popularity_score"`
	Recommended     []Recommendation `json:"recommended"`
}

// RateLimitedError означает, что recommendations сознательно защищает
// себя от перегрузки. Это не технический отказ зависимости.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "recommendations rate limited" }

// RateLimited позволяет retry-циклу и bulkhead отличать 429 от реального сбоя.
func (e *RateLimitedError) RateLimited() bool { return true }

func IsRateLimited(err error) bool {
	var target *RateLimitedError
	return errors.As(err, &target)
}

type RecommendationsClient struct {
	httpClient *http.Client
	baseURL    string
	cb         *gobreaker.CircuitBreaker
	logger     *slog.Logger

	// Учебные паттерны поверх circuit breaker.
	cfg      *resilience.Config
	rMetrics *resilience.Metrics
	fallback *resilience.Fallback[*Response]
	bulkhead *resilience.Bulkhead
}

func stateToFloat(s gobreaker.State) float64 {
	switch s {
	case gobreaker.StateClosed:
		return 0
	case gobreaker.StateHalfOpen:
		return 1
	case gobreaker.StateOpen:
		return 2
	default:
		return -1
	}
}

// NewRecommendationsClient настраивает circuit breaker: после 3 подряд
// неудачных обращений цепь размыкается на 10 секунд (не шлём туда трафик),
// затем пропускает пробные запросы в half-open.
func NewRecommendationsClient(baseURL string, logger *slog.Logger, cfg *resilience.Config, rMetrics *resilience.Metrics) *RecommendationsClient {
	stateGauge := promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "backend",
		Name:      "circuit_breaker_state",
		Help:      "Состояние circuit breaker до recommendations: 0=closed, 1=half-open, 2=open.",
	})
	stateGauge.Set(0)

	settings := gobreaker.Settings{
		Name:        "recommendations",
		MaxRequests: 2,
		Interval:    30 * time.Second,
		Timeout:     10 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		},
		// 429 — ожидаемый защитный ответ от перегруженной зависимости. Если
		// считать его аварией, волна клиентов ошибочно откроет breaker.
		IsSuccessful: func(err error) bool {
			return err == nil || IsRateLimited(err)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warn("circuit_breaker_state_change",
				"dependency", name, "from", from.String(), "to", to.String())
			stateGauge.Set(stateToFloat(to))
		},
	}

	return &RecommendationsClient{
		baseURL: baseURL,
		cb:      gobreaker.NewCircuitBreaker(settings),
		logger:  logger,
		httpClient: &http.Client{
			Timeout: 2 * time.Second, // жёсткий низкоуровневый потолок на одно соединение
			// otelhttp.NewTransport автоматически прокидывает traceparent —
			// именно так span в recommendations попадает в тот же trace.
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		cfg:      cfg,
		rMetrics: rMetrics,
		fallback: resilience.NewFallback[*Response](cfg.FallbackTTL),
		bulkhead: resilience.NewBulkhead(1),
	}
}

// Get возвращает рекомендации, либо ошибку (в т.ч. gobreaker.ErrOpenState,
// если цепь сейчас разомкнута). Вызов обёрнут в механизмы отказоустойчивости.
// Второе возвращаемое значение — outcome: какие паттерны сработали (нужно
// для логов/трейсов, чтобы показать студентам "след" защиты).
func (c *RecommendationsClient) Get(ctx context.Context, productID int) (*Response, *resilience.Outcome, error) {
	value, outcome, err := resilience.ProtectedCall(
		ctx,
		"recommendations",
		c.cfg,
		c.rMetrics,
		c.fallback,
		c.bulkhead,
		c.logger,
		// Ретраим только технические сбои: 5xx и сеть. 429 (reйт-лимит) и
		// бизнес-ответы повторять бессмысленно — иначе получим retry storm.
		func(err error) bool {
			return err != nil && !IsRateLimited(err) && !errors.Is(err, context.Canceled)
		},
		func(ctx context.Context) (*Response, error) {
			return c.getOnce(ctx, productID)
		},
	)
	return value, outcome, err
}

// getOnce — однократный реальный HTTP-вызов через circuit breaker (без
// ретраев и таймаутов — те навешивает ProtectedCall снаружи).
func (c *RecommendationsClient) getOnce(ctx context.Context, productID int) (*Response, error) {
	result, err := c.cb.Execute(func() (interface{}, error) {
		url := fmt.Sprintf("%s/recommendations/%d", c.baseURL, productID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := time.Second
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
				retryAfter = time.Duration(seconds) * time.Second
			}
			return nil, &RateLimitedError{RetryAfter: retryAfter}
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("recommendations returned status %d", resp.StatusCode)
		}

		var out Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		return &out, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*Response), nil
}

// State отдаёт текущее состояние — используется в /healthz для наглядности.
func (c *RecommendationsClient) State() string {
	return c.cb.State().String()
}
