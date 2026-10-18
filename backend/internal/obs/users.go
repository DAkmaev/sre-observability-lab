// Пользовательская сессия и middleware, который доносит identity до всех
// трёх столпов наблюдаемости: user.id прикрепляется к корневому span'у,
// а user_id/user_role — к access-логу. Метрики по user_id НЕ делаем
// (кардинальность) — только трейсы и логи.

package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type User struct {
	ID   int
	Name string
	Role string
}

type userKey struct{}

// UserFromContext возвращает пользователя из контекста запроса и ok=false,
// если запрос анонимный (сессии нет).
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

func withUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// ── In-memory хранилище сессий ────────────────────────────────────────────
// Стенд учебный: перезапуск backend обнуляет сессии (users/отзывы/склад
// остаются в БД). Токен — случайный hex, фронт хранит его в localStorage.
// Сессии живут пока активен клиент и не дольше sessionTTL: у каждой сессии
// есть срок ExpiresAt, который продлевается при Get, а устаревшие сессии
// периодически вычищает goroutine-уборщик (SessionStore.StartJanitor).
// Это гарантирует, что gauge активных сессий падает, даже если клиент
// (например, скрипт) закончился, не позвав DELETE /api/session.

// sessionTTL — время жизни сессии без обращений.
const sessionTTL = 15 * time.Minute

// Session описывает одну активную сессию.
type Session struct {
	Token     string
	User      User
	ExpiresAt time.Time
}

func (s Session) expired() bool {
	return time.Now().After(s.ExpiresAt)
}

// SessionStore хранит token → session в памяти. Потокобезопасен.
// onSize (если задан) вызывается при каждом изменении числа активных
// сессий с актуальным их количеством — нужен для обновления gauge.
func SessionStore(onSize func(int)) *sessionStore {
	return &sessionStore{
		sessions: &sync.Map{},
		onSize:   onSize,
	}
}

type sessionStore struct {
	sessions *sync.Map
	onSize   func(int)
}

// StartJanitor запускает периодическую уборку устаревших сессий. Stop по
// завершении контекста; не блокирует навсегда — для остановки горутины
// достаточно отмены ctx, остальное уборочный цикл беспрепятственно завершит.
func (s *sessionStore) StartJanitor(ctx context.Context) {
	go func() {
		interval := sessionTTL / 3
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.SweepExpired()
			}
		}
	}()
}

// SweepExpired удаляет все истёкшие сессии (вызывается и вручную из тестов).
func (s *sessionStore) SweepExpired() {
	removed := false
	s.sessions.Range(func(k, v interface{}) bool {
		if v.(Session).expired() {
			s.sessions.Delete(k)
			removed = true
		}
		return true
	})
	if removed {
		s.notify()
	}
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create выдаёт новый токен для пользователя.
func (s *sessionStore) Create(user User) string {
	token := randomToken()
	s.sessions.Store(token, Session{Token: token, User: user, ExpiresAt: time.Now().Add(sessionTTL)})
	s.notify()
	return token
}

// Get возвращает сессию по токену и продлевает её срок жизни. Устаревшая
// сессия считается неактивной, пока её не вычистит janitor.
func (s *sessionStore) Get(token string) (Session, bool) {
	v, ok := s.sessions.Load(token)
	if !ok {
		return Session{}, false
	}
	sess := v.(Session)
	if sess.expired() {
		// Немедленно выкидываем и сбрасываем gauge в том же потоке.
		s.sessions.Delete(token)
		s.notify()
		return Session{}, false
	}
	sess.ExpiresAt = time.Now().Add(sessionTTL)
	s.sessions.Store(token, sess)
	return sess, true
}

// Delete удаляет сессию по токену.
func (s *sessionStore) Delete(token string) bool {
	_, ok := s.sessions.LoadAndDelete(token)
	if ok {
		s.notify()
	}
	return ok
}

// ActiveCount возвращает число активных сессий.
func (s *sessionStore) ActiveCount() int {
	n := 0
	s.sessions.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

func (s *sessionStore) notify() {
	if s.onSize == nil {
		return
	}
	s.onSize(s.ActiveCount())
}

// ── Middleware: UserContext ────────────────────────────────────────────────
// Читает заголовок X-User-Name + токен X-Session-Token, сверяет их с
// in-memory сессией и кладёт пользователя в контекст. Прикрепляет user.id к
// корневому span'у (тому, что создаёт otelhttp) — это делает identity видимой
// в трейсах без взрыва label-кардинальности в метриках.
func UserContext(sessions *sessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			name := r.Header.Get("X-User-Name")
			token := r.Header.Get("X-Session-Token")

			// Сессию идентифицирует токен (это и есть учётные данные). Имя
			// — лишь клиентский хинт: если оно пришло и не совпало с сессией,
			// не пускаем; если имя не пришло (например, клиент не восстановил
			// его после перезагрузки), всё равно пропускаем по токену.
			if token != "" {
				if sess, ok := sessions.Get(token); ok && (name == "" || sess.User.Name == name) {
					ctx = withUser(ctx, sess.User)
					// Корневой span создан otelhttp ДО этой middleware
					// (цепочка: otelhttp → WithRequestID → InstrumentServer →
					// UserContext). Обогащаем его прямо из контекста.
					if sc := trace.SpanFromContext(ctx); sc != nil {
						sc.SetAttributes(
							attribute.Int("user.id", sess.User.ID),
							attribute.String("user.role", sess.User.Role),
						)
					}
				}
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
