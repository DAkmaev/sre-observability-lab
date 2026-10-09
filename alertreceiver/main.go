// alertreceiver — webhook-приёмник уведомлений Alertmanager для учебного стенда.
//
// Печатает каждое уведомление (алерт) в stdout в структурированном JSON —
// оттуда его забирает Vector и отправляет в VictoriaLogs, поэтому инциденты
// видны в логах так же, как обычные логи сервисов. Отдаёт 200 OK, чтобы
// Alertmanager не ретраил уведомления.
//
// Зачем нужен отдельный сервис, а не stdout самого Alertmanager: у
// прометеевского alertmanager нет встроенного способа «печатать уведомления»,
// а webhook-приёмник даёт студентам наглядную точку, которую можно заменить
// реальным каналом (Slack/Telegram/email), просто поменяв url на стороне
// Alertmanager.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// alert — минимальный срез уведомления, который мы оставляем в логах.
// Alertmanager шлёт более толстую структуру (group/labels/annotations/
// commonLabels и т.п.), но в stdout выводим только то, что реально нужно
// при разборе инцидента.
type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// webhookBody — конверт webhook-приёмника Alertmanager.
type webhookBody struct {
	Alerts []alert `json:"alerts"`
}

// newHandler собирает HTTP-маршруты сервиса. Вынесено в отдельную функцию,
// чтобы тесты могли проверить обработчик без поднятия полноценного сервера.
func newHandler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Alertmanager дергает POST на /alert. Точка входа webhook'а в
	// alertmanager.yml (receiver lab-webhook → url).
	mux.HandleFunc("POST /alert", func(w http.ResponseWriter, r *http.Request) {
		var body webhookBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			logger.Warn("invalid webhook body",
				"remote_addr", r.RemoteAddr, "error", err)
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		for _, a := range body.Alerts {
			status := a.Status
			if status == "" {
				status = "unknown"
			}
			logger.Info("alert_notification",
				"status", status,
				"alertname", a.Labels["alertname"],
				"severity", a.Labels["severity"],
				"service", a.Labels["service"],
				"title", a.Annotations["title"],
				"description", a.Annotations["description"],
			)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Нечего сделать при ошибке записи в завершающемся соединении.
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Доступность приёмника полезно видеть и в красивой картинке — смотрим
	// up{job="alertreceiver"} в VictoriaMetrics. Метрики минимальные, как и
	// положено простому вспомогательному сервису.
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(
			"# TYPE alertreceiver_up gauge\n" +
				"alertreceiver_up 1\n",
		))
	})

	return mux
}

func main() {
	service := envOr("SERVICE_NAME", "alertreceiver")
	addr := envOr("HTTP_ADDR", ":9119")
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("service", service)

	srv := &http.Server{Addr: addr, Handler: newHandler(logger)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("alertreceiver started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down alertreceiver...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

// envOr возвращает значение переменной окружения name, либо fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
