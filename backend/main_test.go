package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sre-lab/backend/internal/store"
)

// ── fakeStore: реализация store.Storage для юнит-тестов бизнес-логики ─────

// fakeStore имитирует слой хранения, реализуя только те методы, которые
// нужны тестам, а об остальных — паникует (упавший «неожиданный» вызов
// сразу показывает, что тест измеряет не то, что ожидалось).
type fakeStore struct {
	stock map[int]int // productID → текущий остаток

	reserveCalls   []reserveCall
	unreserveCalls []reserveCall

	// errOnReserve / errOnUnreserve — productID, для которого соответствующий
	// вызов падает технической ошибкой (нужно для тестов краевых случаев).
	errOnReserve   map[int]error
	errOnUnreserve map[int]error
}

type reserveCall struct {
	productID int
	qty       int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		stock:          map[int]int{},
		errOnReserve:   map[int]error{},
		errOnUnreserve: map[int]error{},
	}
}

func (f *fakeStore) Ping(context.Context) error { return nil }

func (f *fakeStore) ListProducts(context.Context) ([]store.Product, error) {
	panic("unexpected call")
}

func (f *fakeStore) GetProduct(context.Context, int) (*store.Product, error) {
	panic("unexpected call")
}

func (f *fakeStore) PriceCartItems(context.Context, []store.CartItem) ([]store.PricedItem, int, error) {
	panic("unexpected call")
}

func (f *fakeStore) CreateOrder(context.Context, int, []store.PricedItem, int, string, string, string, string) (*store.Order, error) {
	panic("unexpected call")
}

func (f *fakeStore) ListOrders(context.Context, int, int) ([]store.Order, error) {
	panic("unexpected call")
}

func (f *fakeStore) UpsertUserByName(context.Context, string) (*store.User, error) {
	panic("unexpected call")
}

func (f *fakeStore) GetUserByID(context.Context, int) (*store.User, error) {
	panic("unexpected call")
}

func (f *fakeStore) GetStock(context.Context, int) (int, error) {
	panic("unexpected call")
}

func (f *fakeStore) ReserveStock(ctx context.Context, productID, qty int, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.errOnReserve[productID] != nil {
		return f.errOnReserve[productID]
	}
	f.reserveCalls = append(f.reserveCalls, reserveCall{productID: productID, qty: qty})
	if f.stock[productID] < qty {
		return store.ErrInsufficientStock
	}
	f.stock[productID] -= qty
	return nil
}

func (f *fakeStore) UnreserveStock(ctx context.Context, productID, qty int, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.errOnUnreserve[productID] != nil {
		return f.errOnUnreserve[productID]
	}
	f.unreserveCalls = append(f.unreserveCalls, reserveCall{productID: productID, qty: qty})
	f.stock[productID] += qty
	return nil
}

func (f *fakeStore) Restock(context.Context, int, int, string) error {
	panic("unexpected call")
}

func (f *fakeStore) ListStock(context.Context) ([]store.StockEntry, error) {
	panic("unexpected call")
}

func (f *fakeStore) ListReviews(context.Context, int) ([]store.Review, error) {
	panic("unexpected call")
}

func (f *fakeStore) CreateReview(context.Context, int, int, int, string) (*store.Review, error) {
	panic("unexpected call")
}

// гарантия соответствия интерфейсу на этапе теста
var _ store.Storage = (*fakeStore)(nil)

// ── reserveInventory ───────────────────────────────────────────────────────

func TestReserveInventoryReservesAllItems(t *testing.T) {
	db := newFakeStore()
	db.stock[1] = 10
	db.stock[2] = 5

	items := []store.PricedItem{
		{ProductID: 1, Quantity: 3},
		{ProductID: 2, Quantity: 2},
	}

	reserved, err := reserveInventory(context.Background(), db, items, "req-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reserved) != 2 || reserved[1] != 3 || reserved[2] != 2 {
		t.Fatalf("reserved = %v, want {1:3, 2:2}", reserved)
	}
	if db.stock[1] != 7 || db.stock[2] != 3 {
		t.Fatalf("stock = %v, want {1:7, 2:3}", db.stock)
	}
	if len(db.unreserveCalls) != 0 {
		t.Fatalf("no unreserve expected on success, got %v", db.unreserveCalls)
	}
}

func TestReserveInventoryInsufficientCompensatesReserved(t *testing.T) {
	db := newFakeStore()
	db.stock[1] = 10
	db.stock[2] = 0 // второго товара нет — резервирование должно сорваться

	items := []store.PricedItem{
		{ProductID: 1, Quantity: 3},
		{ProductID: 2, Quantity: 1},
		{ProductID: 3, Quantity: 5}, // не должен даже дойти до вызова
	}

	reserved, err := reserveInventory(context.Background(), db, items, "req-2")
	if !errors.Is(err, store.ErrInsufficientStock) {
		t.Fatalf("expected ErrInsufficientStock, got %v", err)
	}
	if reserved != nil {
		t.Fatalf("reserved should be nil on failure, got %v", reserved)
	}
	// Товар 1 был зарезервирован и должен быть возвращён обратно: остаток снова 10.
	if db.stock[1] != 10 {
		t.Fatalf("product 1 stock = %d, want 10 (compensated)", db.stock[1])
	}
	if got := len(db.unreserveCalls); got != 1 {
		t.Fatalf("expected 1 unreserve (compensation), got %d", got)
	}
	if got := len(db.reserveCalls); got != 2 {
		t.Fatalf("expected 2 reserve attempts, got %d", got)
	}
}

func TestReserveInventoryZeroQuantityItemsSkippedAndNoCalls(t *testing.T) {
	db := newFakeStore()

	items := []store.PricedItem{{ProductID: 9, Quantity: 0}}

	reserved, err := reserveInventory(context.Background(), db, items, "req-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reserved) != 0 {
		t.Fatalf("reserved = %v, want empty", reserved)
	}
	if len(db.reserveCalls) != 0 {
		t.Fatalf("no reserve call expected for zero qty, got %v", db.reserveCalls)
	}
}

func TestReserveInventoryPropagatesTechnicalError(t *testing.T) {
	dbErr := errors.New("db down")
	db := newFakeStore()
	db.errOnReserve[1] = dbErr

	_, err := reserveInventory(context.Background(), db,
		[]store.PricedItem{{ProductID: 1, Quantity: 1}}, "req-4")
	if !errors.Is(err, dbErr) {
		t.Fatalf("want %v, got %v", dbErr, err)
	}
	if len(db.unreserveCalls) != 0 {
		t.Fatalf("technical error must NOT trigger compensation, got %v", db.unreserveCalls)
	}
}

// ── releaseStock ───────────────────────────────────────────────────────────

func TestReleaseStockReturnsAll(t *testing.T) {
	db := newFakeStore()

	reserved := map[int]int{1: 3, 2: 2}
	released, firstErr := releaseStock(context.Background(), db, reserved, "req-5")
	if firstErr != nil {
		t.Fatalf("unexpected error: %v", firstErr)
	}
	if released != 2 {
		t.Fatalf("released = %d, want 2", released)
	}
	if len(db.unreserveCalls) != 2 {
		t.Fatalf("expected 2 unreserve calls, got %v", db.unreserveCalls)
	}
	if db.stock[1] != 3 || db.stock[2] != 2 {
		t.Fatalf("stock = %v, want {1:3, 2:2}", db.stock)
	}
}

func TestReleaseStockContinuesAfterFirstError(t *testing.T) {
	db := newFakeStore()
	reserved := map[int]int{1: 1, 2: 2}

	db.errOnUnreserve[1] = errors.New("db down") // для товара 1 UnreserveStock падает
	released, firstErr := releaseStock(context.Background(), db, reserved, "req-6")
	if firstErr == nil {
		t.Fatal("expected an error for product 1")
	}
	// второй товар всё равно должен быть возвращён
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	if db.stock[2] != 2 {
		t.Fatalf("product 2 stock = %d, want 2 (released despite earlier failure)", db.stock[2])
	}
}

func TestReleaseStockEmptyReturnsZero(t *testing.T) {
	db := newFakeStore()
	released, firstErr := releaseStock(context.Background(), db, map[int]int{}, "req-7")
	if released != 0 || firstErr != nil {
		t.Fatalf("released=%d err=%v, want 0,nil", released, firstErr)
	}
}

// ── orderFilter (видимость истории покупок) ────────────────────────────────

func TestOrderFilter(t *testing.T) {
	tests := []struct {
		name     string
		userID   int
		role     string
		wantFile int
	}{
		{name: "admin sees all", userID: 7, role: "admin", wantFile: 0},
		{name: "regular user sees own", userID: 7, role: "user", wantFile: 7},
		{name: "empty role treats as user", userID: 3, role: "", wantFile: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orderFilter(tt.userID, tt.role); got != tt.wantFile {
				t.Fatalf("orderFilter(%d, %q) = %d, want %d", tt.userID, tt.role, got, tt.wantFile)
			}
		})
	}
}

// ── validateReview ─────────────────────────────────────────────────────────

func TestValidateReview(t *testing.T) {
	tests := []struct {
		name    string
		rating  int
		comment string
		wantErr string
	}{
		{name: "valid", rating: 5, comment: "good", wantErr: ""},
		{name: "min rating", rating: 1, comment: "ok", wantErr: ""},
		{name: "rating too low", rating: 0, comment: "ok", wantErr: "between 1 and 5"},
		{name: "rating too high", rating: 6, comment: "ok", wantErr: "between 1 and 5"},
		{name: "empty comment", rating: 4, comment: "   ", wantErr: "comment must not be empty"},
		{name: "whitespace comment", rating: 4, comment: "  \n\t ", wantErr: "comment must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReview(tt.rating, tt.comment)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
