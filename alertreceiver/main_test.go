package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlertHandler(t *testing.T) {
	t.Run("parses valid webhook and logs each alert", func(t *testing.T) {
		buf := &bytes.Buffer{}
		logger := slog.New(slog.NewTextHandler(buf, nil))

		mux := newHandler(logger)

		body := `{"alerts":[{"status":"firing","labels":{"alertname":"TargetDown","severity":"critical","service":"backend"},"annotations":{"title":"t","description":"d"}}]}`
		req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader(body))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		out := buf.String()
		if !strings.Contains(out, "alert_notification") {
			t.Errorf("log does not contain message alert_notification: %q", out)
		}
		if !strings.Contains(out, "TargetDown") {
			t.Errorf("log does not contain alertname TargetDown: %q", out)
		}
		if !strings.Contains(out, "firing") {
			t.Errorf("log does not contain status firing: %q", out)
		}
	})

	t.Run("rejects invalid JSON with 400", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
		mux := newHandler(logger)

		req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader("not json"))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("status defaults to unknown when empty", func(t *testing.T) {
		buf := &bytes.Buffer{}
		logger := slog.New(slog.NewTextHandler(buf, nil))
		mux := newHandler(logger)

		body := `{"alerts":[{"labels":{"alertname":"X"}}]}`
		req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader(body))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if !strings.Contains(buf.String(), "unknown") {
			t.Errorf("expected status to default to unknown, got: %q", buf.String())
		}
	})
}

func TestAlertReceiverMetrics(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	mux := newHandler(logger)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "alertreceiver_up 1") {
		t.Errorf("metrics missing alertreceiver_up: %q", rr.Body.String())
	}
}
