package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVisitorStoreTouchNewAndExisting(t *testing.T) {
	var notified int
	store := VisitorStore(func(n int) { notified = n })

	// Новый посетитель — счётчик растёт до 1, выдаётся cookie.
	id := store.Touch("")
	if id == "" {
		t.Fatal("expected a non-empty visitor id")
	}
	if notified != 1 {
		t.Fatalf("expected 1 active visitor callback, got %d", notified)
	}
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", store.ActiveCount())
	}

	// Повторное обращение с тем же ID — счётчик не растёт.
	if got := store.Touch(id); got != id {
		t.Fatalf("Touch with existing id returned %q, want %q", got, id)
	}
	if notified != 1 {
		t.Fatalf("expected callback to stay 1 on touch, got %d", notified)
	}
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1 after repeated touch", store.ActiveCount())
	}
}

func TestVisitorStoreExpiredRemoved(t *testing.T) {
	store := VisitorStore(func(n int) {})

	id := store.Touch("")
	store.visitors.Store(id, Visitor{ID: id, ExpiresAt: time.Now().Add(-time.Minute)})

	// Протухший ID в Touch должен восприниматься как новый посетитель:
	// старый удаляется, выдаётся свежий.
	newID := store.Touch(id)
	if newID == id {
		t.Fatal("Touch must issue a fresh id for an expired visitor")
	}
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", store.ActiveCount())
	}
}

func TestVisitorStoreSweepExpired(t *testing.T) {
	var notified int
	store := VisitorStore(func(n int) { notified = n })

	live := store.Touch("")
	stale := store.Touch("")
	if notified != 2 {
		t.Fatalf("expected 2 active visitors callback, got %d", notified)
	}

	store.visitors.Store(stale, Visitor{ID: stale, ExpiresAt: time.Now().Add(-time.Minute)})
	store.SweepExpired()

	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1 after sweep", store.ActiveCount())
	}
	if _, ok := store.visitors.Load(live); !ok {
		t.Fatal("live visitor must survive sweep")
	}
	if notified != 1 {
		t.Fatalf("expected gauge notified to 1 after sweep, got %d", notified)
	}
}

func TestVisitorStoreJanitorStopsOnCancel(t *testing.T) {
	store := VisitorStore(nil)
	ctx, cancel := context.WithCancel(context.Background())
	store.StartJanitor(ctx)
	cancel()
	// Нет паники/утечки — контекст отменён, горутина завершается.
}

func TestTrackVisitorsIssuesAndReusesCookie(t *testing.T) {
	store := VisitorStore(nil)
	h := TrackVisitors(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Первый запрос — cookie выдан.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/products", nil)
	h.ServeHTTP(rec, req)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("expected a visitor cookie to be set")
	}
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1 after first visit", store.ActiveCount())
	}

	// Второй запрос с тем же cookie — счётчик не растёт.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/products", nil)
	req2.AddCookie(cookie)
	h.ServeHTTP(rec2, req2)
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1 after repeated visit", store.ActiveCount())
	}
}

func TestTrackVisitorsSkipsMetrics(t *testing.T) {
	store := VisitorStore(nil)
	h := TrackVisitors(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)

	if store.ActiveCount() != 0 {
		t.Fatalf("/metrics must not count as a visitor, ActiveCount = %d", store.ActiveCount())
	}
	for _, cc := range rec.Result().Cookies() {
		if cc.Name == cookieName {
			t.Fatal("/metrics must not issue a visitor cookie")
		}
	}
}
