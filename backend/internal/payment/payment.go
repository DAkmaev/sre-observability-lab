// Package payment — HTTP-клиент до payment-сервиса, обёрнутый в свой
// собственный circuit breaker (по аналогии с internal/breaker для
// recommendations). Отдельный breaker, потому что оплата и
// рекомендации — независимые зависимости: то, что "упал" сервис
// рекомендаций, не должно останавливать оплату, и наоборот.
//
// Поверх breaker навиваются механизмы отказоустойчивости (bulkhead, retry,
// таймаут, fallback) — так же, как у recommendations.
package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"sre-lab/backend/internal/resilience"
)

type ChargeRequest struct {
	OrderRef    string `json:"order_ref"`
	AmountCents int    `json:"amount_cents"`
}

type ChargeResponse struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"` // approved | declined | error
	Reason    string `json:"reason,omitempty"`
}

// BusinessDecline — ошибка "карта отклонена банком": штатная ситуация,
// circuit breaker НЕ должен считать это отказом зависимости (это не она
// сломалась, это конкретная карта не прошла). Отличать такие ошибки от
// технических (таймаут, 5xx, недоступность БД) — ровно то умение, которое
// нужно для лабы: неправильная реакция здесь превратила бы единичный
// отказ одной карты в открытие цепи для всех покупателей.
type BusinessDecline struct {
	Reason string
}

func (e *BusinessDecline) Error() string { return "payment declined: " + e.Reason }

// Retryable сообщает retry-циклу, что бизнес-отказ повторять не нужно.
func (e *BusinessDecline) Retryable() bool { return false }

type Client struct {
	httpClient *http.Client
	baseURL    string
	cb         *gobreaker.CircuitBreaker
	logger     *slog.Logger

	cfg      *resilience.Config
	rMetrics *resilience.Metrics
	fallback *resilience.Fallback[*ChargeResponse]
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

// NewClient настраивает circuit breaker для payment: 3 подряд технических
// отказа (таймаут/5xx/недоступность БД) размыкают цепь на 10 секунд.
// Бизнес-отказы карты (BusinessDecline) в счётчик отказов НЕ попадают —
// см. IsSuccessful ниже.
func NewClient(baseURL string, logger *slog.Logger, cfg *resilience.Config, rMetrics *resilience.Metrics) *Client {
	stateGauge := promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "backend",
		Name:      "payment_circuit_breaker_state",
		Help:      "Состояние circuit breaker до payment: 0=closed, 1=half-open, 2=open.",
	})
	stateGauge.Set(0)

	settings := gobreaker.Settings{
		Name:        "payment",
		MaxRequests: 2,
		Interval:    30 * time.Second,
		Timeout:     10 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		},
		// IsSuccessful решает, что считать отказом для целей breaker'а.
		// Бизнес-отказ карты (BusinessDecline) — это успешный вызов
		// зависимости, которая просто ответила "нет". Считать его отказом
		// значило бы размыкать цепь из-за того, что кто-то ввёл неверный
		// CVV — а это не то, от чего защищает circuit breaker.
		IsSuccessful: func(err error) bool {
			if err == nil {
				return true
			}
			_, isBusiness := err.(*BusinessDecline)
			return isBusiness
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warn("circuit_breaker_state_change",
				"dependency", name, "from", from.String(), "to", to.String())
			stateGauge.Set(stateToFloat(to))
		},
	}

	return &Client{
		baseURL: baseURL,
		cb:      gobreaker.NewCircuitBreaker(settings),
		logger:  logger,
		httpClient: &http.Client{
			Timeout: 3 * time.Second, // низкоуровневый потолок на одно соединение
			// otelhttp.NewTransport прокидывает traceparent — span в
			// payment-сервисе попадает в тот же trace, что и checkout.
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		cfg:      cfg,
		rMetrics: rMetrics,
		fallback: resilience.NewFallback[*ChargeResponse](cfg.FallbackTTL),
		bulkhead: resilience.NewBulkhead(1),
	}
}

// Charge вызывает POST /payments/charge через механизмы отказоустойчивости.
// Возвращает *BusinessDecline, если банк отклонил карту (штатная ситуация —
// не деградация payment-сервиса), либо обычную ошибку при технических
// проблемах (таймаут, 5xx, разомкнутая цепь — gobreaker.ErrOpenState).
// Второе возвращаемое значение — outcome сработавших паттернов.
func (c *Client) Charge(ctx context.Context, orderRef string, amountCents int) (*ChargeResponse, *resilience.Outcome, error) {
	value, outcome, err := resilience.ProtectedCall(
		ctx,
		"payment",
		c.cfg,
		c.rMetrics,
		c.fallback,
		c.bulkhead,
		c.logger,
		// Ретраим только технические сбои. Бизнес-отказ карты повторять нельзя
		// (это не временный сбой зависимости), иначе заплатим дважды.
		func(err error) bool {
			if err == nil {
				return false
			}
			var decline *BusinessDecline
			if errors.As(err, &decline) {
				return false
			}
			return !errors.Is(err, context.Canceled)
		},
		func(ctx context.Context) (*ChargeResponse, error) {
			return c.chargeOnce(ctx, orderRef, amountCents)
		},
	)
	return value, outcome, err
}

// chargeOnce — однократный реальный вызов через circuit breaker.
func (c *Client) chargeOnce(ctx context.Context, orderRef string, amountCents int) (*ChargeResponse, error) {
	result, err := c.cb.Execute(func() (interface{}, error) {
		body, err := json.Marshal(ChargeRequest{OrderRef: orderRef, AmountCents: amountCents})
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s/payments/charge", c.baseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var out ChargeResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode payment response: %w", err)
		}

		if resp.StatusCode == http.StatusOK && out.Status == "declined" {
			return &out, &BusinessDecline{Reason: out.Reason}
		}
		if resp.StatusCode != http.StatusOK {
			return &out, fmt.Errorf("payment service returned status %d (reason=%s)", resp.StatusCode, out.Reason)
		}
		return &out, nil
	})

	// result может быть заполнен даже при ошибке (например, BusinessDecline) —
	// возвращаем его вызывающему коду, чтобы он мог показать причину отказа.
	if result != nil {
		if resp, ok := result.(*ChargeResponse); ok && resp != nil {
			return resp, err
		}
	}
	return nil, err
}

// State отдаёт текущее состояние — пригодится в /healthz и в логах.
func (c *Client) State() string {
	return c.cb.State().String()
}
