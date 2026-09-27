// Package store — тонкий слой доступа к Postgres.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Product struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int    `json:"price_cents"`
	Emoji       string `json:"emoji"`
	Category    string `json:"category"`
	Stock       int    `json:"stock"`
	InStock     bool   `json:"in_stock"`
	// AvgRating и RatingCount агрегируются по отзывам. По умолчанию —
	// пустые значения, чтобы товар без отзывов вел отображение корректно.
	AvgRating   float64 `json:"avg_rating"`
	RatingCount int     `json:"rating_count"`
}

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type Review struct {
	ID        int       `json:"id"`
	ProductID int       `json:"product_id"`
	UserID    int       `json:"user_id"`
	UserName  string    `json:"user_name,omitempty"`
	Rating    int       `json:"rating"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrInsufficientStock — штатное бизнес-состояние: товара не хватает на
// резервирование. НЕ является технической ошибкой БД, обрабатывается в
// checkout как 409.
var ErrInsufficientStock = errors.New("insufficient stock")

type Store struct {
	pool *pgxpool.Pool
}

// Storage — набор операций слоя хранения, которые требуют HTTP-хендлеры
// backend'а. Определён на стороне потребителя (main.go), а *Store реализует
// его неявно. Это позволяет тестировать бизнес-логику хендлеров на fake-репозитории
// без реальной БД/сети (правило проекта: чистые юнит-тесты для логики).
type Storage interface {
	Ping(ctx context.Context) error

	ListProducts(ctx context.Context) ([]Product, error)
	GetProduct(ctx context.Context, id int) (*Product, error)
	PriceCartItems(ctx context.Context, items []CartItem) ([]PricedItem, int, error)
	CreateOrder(ctx context.Context, userID int, items []PricedItem, totalCents int, status, paymentID, failureReason, requestID string) (*Order, error)
	// ListOrders возвращает заказы. filterUserID ≠ 0 → только заказы этого
	// покупателя; filterUserID == 0 → все заказы (админ). limit ограничивает
	// количество (≤0 снимает лимит).
	ListOrders(ctx context.Context, filterUserID int, limit int) ([]Order, error)

	UpsertUserByName(ctx context.Context, name string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)

	GetStock(ctx context.Context, productID int) (int, error)
	ReserveStock(ctx context.Context, productID, qty int, requestID string) error
	UnreserveStock(ctx context.Context, productID, qty int, requestID string) error
	Restock(ctx context.Context, productID, qty int, requestID string) error
	ListStock(ctx context.Context) ([]StockEntry, error)

	ListReviews(ctx context.Context, productID int) ([]Review, error)
	CreateReview(ctx context.Context, productID, userID, rating int, comment string) (*Review, error)
}

// Компиляционная проверка: *Store всегда удовлетворяет Storage.
var _ Storage = (*Store)(nil)

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) ListProducts(ctx context.Context) ([]Product, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.name, p.description, p.price_cents, p.emoji, p.category, p.stock,
		        coalesce(avg(r.rating)::float, 0), count(r.id)
		 FROM products p
		 LEFT JOIN reviews r ON r.product_id = p.id
		 GROUP BY p.id
		 ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.PriceCents, &p.Emoji, &p.Category,
			&p.Stock, &p.AvgRating, &p.RatingCount); err != nil {
			return nil, err
		}
		p.InStock = p.Stock > 0
		products = append(products, p)
	}
	return products, rows.Err()
}

func (s *Store) GetProduct(ctx context.Context, id int) (*Product, error) {
	var p Product
	err := s.pool.QueryRow(ctx,
		`SELECT p.id, p.name, p.description, p.price_cents, p.emoji, p.category, p.stock,
		        coalesce(avg(r.rating)::float, 0), count(r.id)
		 FROM products p
		 LEFT JOIN reviews r ON r.product_id = p.id
		 WHERE p.id = $1
		 GROUP BY p.id`,
		id,
	).Scan(&p.ID, &p.Name, &p.Description, &p.PriceCents, &p.Emoji, &p.Category,
		&p.Stock, &p.AvgRating, &p.RatingCount)
	if err != nil {
		return nil, err
	}
	p.InStock = p.Stock > 0
	return &p, nil
}

// ── Пользователи (лёгкая аутентификация для лабы) ────────────────────────

// UpsertUserByName находит пользователя по имени, создавая его с ролью
// 'user', если он ещё не существует. Админ 'admin' уже засеян в init.sql.
func (s *Store) UpsertUserByName(ctx context.Context, name string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (name, role)
		 VALUES ($1, 'user')
		 ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		 RETURNING id, name, role`,
		name,
	).Scan(&u.ID, &u.Name, &u.Role)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id int) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, role FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Name, &u.Role)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ── Склад (остатки и ledger stock_movements) ──────────────────────────────

// GetStock возвращает текущий остаток товара.
func (s *Store) GetStock(ctx context.Context, productID int) (int, error) {
	var stock int
	err := s.pool.QueryRow(ctx,
		`SELECT stock FROM products WHERE id = $1`, productID,
	).Scan(&stock)
	if err != nil {
		return 0, err
	}
	return stock, nil
}

// ReserveStock атомарно списывает qty с остатка в транзакции и пишет в
// stock_movements с reason="reserve". Если товара не хватает, ничего не
// меняется и возвращается ErrInsufficientStock. Паттерн транзакции тот же,
// что и в CreateOrder: Begin → defer Rollback → Commit.
func (s *Store) ReserveStock(ctx context.Context, productID, qty int, requestID string) error {
	if qty <= 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var before, after int
	err = tx.QueryRow(ctx,
		`UPDATE products SET stock = stock - $1
		 WHERE id = $2 AND stock >= $1
		 RETURNING stock + $1, stock`,
		qty, productID,
	).Scan(&before, &after)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInsufficientStock
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO stock_movements (product_id, qty_delta, before_stock, after_stock, reason, request_id)
		 VALUES ($1, $2, $3, $4, 'reserve', $5)`,
		productID, -qty, before, after, requestID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// UnreserveStock возвращает резерв на склад (компенсирующее действие, когда
// оплата после резервирования не прошла). Пишет движение c reason="unreserve".
func (s *Store) UnreserveStock(ctx context.Context, productID, qty int, requestID string) error {
	if qty <= 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var before, after int
	err = tx.QueryRow(ctx,
		`UPDATE products SET stock = stock + $1
		 WHERE id = $2
		 RETURNING stock - $1, stock`,
		qty, productID,
	).Scan(&before, &after)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO stock_movements (product_id, qty_delta, before_stock, after_stock, reason, request_id)
		 VALUES ($1, $2, $3, $4, 'unreserve', $5)`,
		productID, qty, before, after, requestID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Restock увеличивает остаток товара (только через админ-панель).
// Повторный restock легален — каждое восполнение пишет свой increment в
// stock_movements.
func (s *Store) Restock(ctx context.Context, productID, qty int, requestID string) error {
	if qty <= 0 {
		return fmt.Errorf("restock quantity must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var before, after int
	err = tx.QueryRow(ctx,
		`UPDATE products SET stock = stock + $1
		 WHERE id = $2
		 RETURNING stock - $1, stock`,
		qty, productID,
	).Scan(&before, &after)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO stock_movements (product_id, qty_delta, before_stock, after_stock, reason, request_id)
		 VALUES ($1, $2, $3, $4, 'restock', $5)`,
		productID, qty, before, after, requestID,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ListStock возвращает текущие остатки по всем товарам — для
// Inventory-карточки в админ-панели.
type StockEntry struct {
	ProductID   int     `json:"product_id"`
	Name        string  `json:"name"`
	Emoji       string  `json:"emoji"`
	Stock       int     `json:"stock"`
	InStock     bool    `json:"in_stock"`
	AvgRating   float64 `json:"avg_rating"`
	RatingCount int     `json:"rating_count"`
}

func (s *Store) ListStock(ctx context.Context) ([]StockEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.name, p.emoji, p.stock,
		        coalesce(avg(r.rating)::float, 0), count(r.id)
		 FROM products p
		 LEFT JOIN reviews r ON r.product_id = p.id
		 GROUP BY p.id
		 ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StockEntry
	for rows.Next() {
		var e StockEntry
		if err := rows.Scan(&e.ProductID, &e.Name, &e.Emoji, &e.Stock, &e.AvgRating, &e.RatingCount); err != nil {
			return nil, err
		}
		e.InStock = e.Stock > 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── Отзывы и рейтинги ─────────────────────────────────────────────────────

func (s *Store) ListReviews(ctx context.Context, productID int) ([]Review, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.product_id, r.user_id, coalesce(u.name, ''),
		        r.rating, r.comment, r.created_at
		 FROM reviews r
		 LEFT JOIN users u ON u.id = r.user_id
		 WHERE r.product_id = $1
		 ORDER BY r.created_at DESC, r.id DESC`,
		productID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.ProductID, &r.UserID, &r.UserName,
			&r.Rating, &r.Comment, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateReview сохраняет отзыв. Валидация диапазона рейтинга и непустого
// комментария выполняется на уровне бизнес-логики (main.go); здесь — только
// честные ограничения целостности через параметризованный запрос.
func (s *Store) CreateReview(ctx context.Context, productID, userID, rating int, comment string) (*Review, error) {
	var r Review
	err := s.pool.QueryRow(ctx,
		`INSERT INTO reviews (product_id, user_id, rating, comment)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, product_id, user_id, rating, comment, created_at`,
		productID, userID, rating, comment,
	).Scan(&r.ID, &r.ProductID, &r.UserID, &r.Rating, &r.Comment, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ── Заказы (корзина → "Купить") ──────────────────────────────────────────

// CartItem — то, что приходит от фронтенда при оформлении заказа: id
// товара и количество. Цену бэкенд всегда берёт из своей БД, а не верит
// клиенту — иначе покупатель мог бы прислать любую цену.
type CartItem struct {
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

// PricedItem — товар из корзины, но уже с ценой и названием, подтянутыми
// из БД (нужно и для сохранения order_items, и для ответа фронтенду).
type PricedItem struct {
	ProductID  int    `json:"product_id"`
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	PriceCents int    `json:"price_cents"`
}

type Order struct {
	ID            int          `json:"id"`
	UserID        int          `json:"user_id,omitempty"`
	UserName      string       `json:"user_name,omitempty"`
	Status        string       `json:"status"` // paid | failed
	TotalCents    int          `json:"total_cents"`
	PaymentID     string       `json:"payment_id,omitempty"`
	FailureReason string       `json:"failure_reason,omitempty"`
	Items         []PricedItem `json:"items"`
	CreatedAt     time.Time    `json:"created_at"`
}

// PriceCartItems подтягивает актуальные цены/названия товаров из БД для
// присланного фронтендом списка id+qty. Возвращает ошибку, если какого-то
// товара уже не существует (например, его удалили из каталога).
func (s *Store) PriceCartItems(ctx context.Context, items []CartItem) ([]PricedItem, int, error) {
	priced := make([]PricedItem, 0, len(items))
	total := 0
	for _, it := range items {
		if it.Quantity <= 0 {
			continue
		}
		var name string
		var priceCents int
		err := s.pool.QueryRow(ctx,
			`SELECT name, price_cents FROM products WHERE id = $1`, it.ProductID,
		).Scan(&name, &priceCents)
		if err != nil {
			return nil, 0, err
		}
		priced = append(priced, PricedItem{
			ProductID:  it.ProductID,
			Name:       name,
			Quantity:   it.Quantity,
			PriceCents: priceCents,
		})
		total += priceCents * it.Quantity
	}
	return priced, total, nil
}

// CreateOrder сохраняет заказ и его позиции одной транзакцией. userID —
// покупатель (заказ всегда создаётся авторизованным пользователем). paymentID
// и failureReason опциональны — заказ создаётся с итоговым статусом уже
// после того, как известен результат обращения к payment-сервису
// (backend сам решает: paid или failed, см. main.go).
func (s *Store) CreateOrder(ctx context.Context, userID int, items []PricedItem, totalCents int, status, paymentID, failureReason, requestID string) (*Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var order Order
	err = tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, status, total_cents, payment_id, failure_reason, request_id)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)
		 RETURNING id, user_id, status, total_cents, coalesce(payment_id, ''), coalesce(failure_reason, ''), created_at`,
		userID, status, totalCents, paymentID, failureReason, requestID,
	).Scan(&order.ID, &order.UserID, &order.Status, &order.TotalCents, &order.PaymentID, &order.FailureReason, &order.CreatedAt)
	if err != nil {
		return nil, err
	}

	for _, it := range items {
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_items (order_id, product_id, quantity, price_cents) VALUES ($1, $2, $3, $4)`,
			order.ID, it.ProductID, it.Quantity, it.PriceCents,
		); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	order.Items = items
	return &order, nil
}

// ListOrders возвращает заказы вместе с их позициями начиная с самых новых.
// filterUserID ≠ 0 — только заказы указанного покупателя (обычный пользователь
// видит свои); filterUserID == 0 — все заказы (админ). limit ограничивает
// количество заказов (0/отрицательное снимает лимит). Позиции заказов содержат
// название товара слева join с products; имя покупателя подтягивается из users.
func (s *Store) ListOrders(ctx context.Context, filterUserID int, limit int) ([]Order, error) {
	base := `
		SELECT o.id, o.user_id, coalesce(u.name, ''), o.status, o.total_cents,
			   coalesce(o.payment_id, ''), coalesce(o.failure_reason, ''), o.created_at
		FROM orders o
		LEFT JOIN users u ON u.id = o.user_id`

	if filterUserID != 0 {
		base += " WHERE o.user_id = " + strconv.Itoa(filterUserID)
	}

	base += " ORDER BY o.created_at DESC, o.id DESC"

	if limit > 0 {
		base += " LIMIT " + strconv.Itoa(limit)
	}

	rows, err := s.pool.Query(ctx, base)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.UserName, &o.Status, &o.TotalCents,
			&o.PaymentID, &o.FailureReason, &o.CreatedAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	itemsRows, err := s.pool.Query(ctx,
		`SELECT i.order_id, p.id, coalesce(p.name, ''), i.quantity, i.price_cents
		 FROM order_items i
		 LEFT JOIN products p ON p.id = i.product_id
		 ORDER BY i.order_id`)
	if err != nil {
		return nil, err
	}
	defer itemsRows.Close()

	for itemsRows.Next() {
		var orderID int
		var item PricedItem
		if err := itemsRows.Scan(&orderID, &item.ProductID, &item.Name, &item.Quantity, &item.PriceCents); err != nil {
			return nil, err
		}
		if idx, ok := func() (int, bool) {
			for i, o := range orders {
				if o.ID == orderID {
					return i, true
				}
			}
			return -1, false
		}(); ok {
			orders[idx].Items = append(orders[idx].Items, item)
		}
	}
	if err := itemsRows.Err(); err != nil {
		return nil, err
	}

	return orders, nil
}
