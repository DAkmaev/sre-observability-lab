package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionStoreCreateGetDeleteActiveCount(t *testing.T) {
	var notified int
	store := SessionStore(func(n int) { notified = n })

	token := store.Create(User{ID: 1, Name: "alice", Role: "user"})
	if token == "" {
		t.Fatal("expected a non-empty token")
	}
	if notified != 1 {
		t.Fatalf("expected 1 active session callback, got %d", notified)
	}
	if store.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", store.ActiveCount())
	}

	sess, ok := store.Get(token)
	if !ok {
		t.Fatal("expected session to be retrieveable by token")
	}
	if sess.User.Name != "alice" || sess.User.Role != "user" {
		t.Fatalf("unexpected session user: %+v", sess.User)
	}

	if _, ok := store.Get("bogus"); ok {
		t.Fatal("bogus token must not resolve")
	}

	if !store.Delete(token) {
		t.Fatal("expected Delete to report removal")
	}
	if notified != 0 {
		t.Fatalf("expected 0 active sessions callback after delete, got %d", notified)
	}
	if store.Delete(token) {
		t.Fatal("second Delete should return false")
	}
	if store.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0 after delete", store.ActiveCount())
	}
}

func TestSessionStoreTokensUnique(t *testing.T) {
	store := SessionStore(nil)
	a := store.Create(User{ID: 1, Name: "a", Role: "user"})
	b := store.Create(User{ID: 2, Name: "b", Role: "user"})
	if a == b {
		t.Fatal("tokens must be unique")
	}
}

func TestUserContextAttachesUser(t *testing.T) {
	store := SessionStore(nil)
	token := store.Create(User{ID: 7, Name: "admin", Role: "admin"})

	var gotFromCtx User
	var ok bool

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFromCtx, ok = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	h := UserContext(store)(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/products", nil)
	req.Header.Set("X-User-Name", "admin")
	req.Header.Set("X-Session-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !ok {
		t.Fatal("expected user in context when valid session+name provided")
	}
	if gotFromCtx.ID != 7 || gotFromCtx.Name != "admin" || gotFromCtx.Role != "admin" {
		t.Fatalf("unexpected user: %+v", gotFromCtx)
	}
}

func TestUserContextRejectsUnknownSession(t *testing.T) {
	store := SessionStore(nil)

	var ok bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok = UserFromContext(r.Context())
	})
	h := UserContext(store)(inner)

	// Верный токен, но другой юзер и wrong name.
	token := store.Create(User{ID: 1, Name: "alice", Role: "user"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Name", "bob")
	req.Header.Set("X-Session-Token", token)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if ok {
		t.Fatal("user must not be attached when session belongs to a different name")
	}
}

func TestUserFromContextMissing(t *testing.T) {
	switch _, ok := UserFromContext(context.Background()); ok {
	case true:
		t.Fatal("expected ok=false for empty context")
	}
}
