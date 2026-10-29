// Активные сессии по активности посетителя, а не по авторизации.
//
// Отличие от sessionStore (auth): здесь сессия считается активной, пока от
// клиента приходят запросы, независимо от того, вошёл он или нет. Каждому
// посетителю выдаётся анонимный cookie; любой запрос касается сессии и
// сдвигает её срок жизни (TTL), а протухшие сессии вычищает janitor.

package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// visitorTTL — время жизни сессии посетителя без обращений.
const visitorTTL = 10 * time.Second

// cookieName — имя cookie с анонимным ID посетителя.
const cookieName = "sre_visitor"

// Visitor описывает одну активную сессию посетителя.
type Visitor struct {
	ID        string
	ExpiresAt time.Time
}

func (v Visitor) expired() bool {
	return time.Now().After(v.ExpiresAt)
}

// VisitorStore хранит id → visitor в памяти. Потокобезопасен.
// onSize (если задан) вызывается при изменении числа активных посетителей
// с актуальным их количеством — нужен для обновления gauge.
func VisitorStore(onSize func(int)) *visitorStore {
	return &visitorStore{
		visitors: &sync.Map{},
		onSize:   onSize,
	}
}

type visitorStore struct {
	visitors *sync.Map
	onSize   func(int)
}

// StartJanitor запускает периодическую уборку протухших сессий посетителей.
func (s *visitorStore) StartJanitor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(visitorTTL / 3)
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

// SweepExpired удаляет все истёкшие сессии посетителей.
func (s *visitorStore) SweepExpired() {
	removed := false
	s.visitors.Range(func(k, v interface{}) bool {
		if v.(Visitor).expired() {
			s.visitors.Delete(k)
			removed = true
		}
		return true
	})
	if removed {
		s.notify()
	}
}

func randomVisitorID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Touch регистрирует обращение посетителя: если сессии ещё нет (или уже
// протухла), создаёт её (счётчик +1), иначе только сдвигает TTL. Возвращает
// ID сессии, чтобы middleware выставил cookie.
func (s *visitorStore) Touch(id string) string {
	if id != "" {
		if v, ok := s.visitors.Load(id); ok {
			sess := v.(Visitor)
			if !sess.expired() {
				sess.ExpiresAt = time.Now().Add(visitorTTL)
				s.visitors.Store(id, sess)
				return id
			}
			s.visitors.Delete(id)
		}
	}
	id = randomVisitorID()
	s.visitors.Store(id, Visitor{ID: id, ExpiresAt: time.Now().Add(visitorTTL)})
	s.notify()
	return id
}

// Delete удаляет сессию посетителя по ID.
func (s *visitorStore) Delete(id string) bool {
	_, ok := s.visitors.LoadAndDelete(id)
	if ok {
		s.notify()
	}
	return ok
}

// ActiveCount возвращает число активных посетителей.
func (s *visitorStore) ActiveCount() int {
	n := 0
	s.visitors.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

func (s *visitorStore) notify() {
	if s.onSize == nil {
		return
	}
	s.onSize(s.ActiveCount())
}

// TrackVisitors — middleware: выдаёт анонимный cookie посетителю и
// регистрирует его активную сессию. /metrics исключаем, чтобы сбор метрик
// не «оживлял» счётчик своими опросами.
func TrackVisitors(visitors *visitorStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			var id string
			if c, err := r.Cookie(cookieName); err == nil {
				id = c.Value
			}
			id = visitors.Touch(id)
			http.SetCookie(w, &http.Cookie{
				Name:   cookieName,
				Value:  id,
				Path:   "/",
				MaxAge: int(visitorTTL.Seconds()),
			})
			next.ServeHTTP(w, r)
		})
	}
}
