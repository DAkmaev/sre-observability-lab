// Бизнес-метрики пользователей, склада и отзывов в namespace "backend".
// Регистрируются так же, как в resilience.NewMetrics: молча игнорируя
// AlreadyRegisteredError (безопасно при повторном создании, например в
// тестах). RED-метрики по новым маршрутам получаются автоматически через
// InstrumentServer — здесь только дополняющие бизнес-счётчики.

package obs

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// BusinessMetrics хранит бизнес-счётчики пользователей/склада/отзывов.
type BusinessMetrics struct {
	// Пользователи и сессии.
	UserLogins     prometheus.Counter
	UserLogouts    prometheus.Counter
	ActiveSessions prometheus.Gauge
	UserActions    *prometheus.CounterVec // by action (не по user_id!)
	AdminDenied    prometheus.Counter

	// Склад.
	OrdersBlockedByStock prometheus.Counter
	InventoryRestock     prometheus.Counter
	OutOfStockEvents     prometheus.Counter
	LowStockProducts     prometheus.Gauge
	StockMovement        *prometheus.CounterVec // by reason

	// Отзывы.
	ReviewPosted   prometheus.Counter
	ReviewRejected prometheus.Counter
	ReviewRating   *prometheus.CounterVec // by rating 1..5
}

func newCounter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: "backend", Name: name, Help: help})
	registerBM(c)
	return c
}

func newGauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "backend", Name: name, Help: help})
	registerBM(g)
	return g
}

func newCounterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "backend", Name: name, Help: help}, labels)
	registerBM(c)
	return c
}

func registerBM(c prometheus.Collector) {
	if err := prometheus.Register(c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return
		}
		panic(err)
	}
}

// NewBusinessMetrics создаёт и регистрирует бизнес-метрики.
func NewBusinessMetrics() *BusinessMetrics {
	return &BusinessMetrics{
		UserLogins:           newCounter("user_logins_total", "Сколько раз пользователи вошли (вход по имени)."),
		UserLogouts:          newCounter("user_logouts_total", "Сколько раз пользователи вышли."),
		ActiveSessions:       newGauge("active_sessions", "Число активных in-memory сессий."),
		UserActions:          newCounterVec("user_actions_total", "Действия пользователей по типу (ТОЛЬКО action, не user_id).", "action"),
		AdminDenied:          newCounter("admin_denied_total", "Сколько раз не-админу отказано в доступе к админ-ручке."),
		OrdersBlockedByStock: newCounter("orders_blocked_by_stock_total", "Заказы, заблокированные нехваткой склада (409)."),
		InventoryRestock:     newCounter("inventory_restock_total", "Восполнения остатков через админ-панель."),
		OutOfStockEvents:     newCounter("out_of_stock_events_total", "События перехода товара в состояние 'нет в наличии'."),
		LowStockProducts:     newGauge("low_stock_products", "Число товаров с нулевым остатком."),
		StockMovement:        newCounterVec("stock_movement_total", "Движения остатков по причине.", "reason"),
		ReviewPosted:         newCounter("reviews_posted_total", "Отзывы, успешно созданные."),
		ReviewRejected:       newCounter("reviews_rejected_total", "Отзывы, отклонённые валидацией (рейтинг/пустой комментарий)."),
		ReviewRating:         newCounterVec("reviews_by_rating_total", "Опубликованные отзывы по оценке.", "rating"),
	}
}

// UpdateGaugeCount обновляет датчик активных сессий текущим значением.
func (b *BusinessMetrics) SetActiveSessions(n int) {
	if b == nil || b.ActiveSessions == nil {
		return
	}
	b.ActiveSessions.Set(float64(n))
}
