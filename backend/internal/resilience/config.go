package resilience

import (
	"sync"
	"time"
)

// Config — динамические настройки учебных паттернов отказоустойчивости.
// Один экземпляр на backend: админ-ручки меняют его на лету, клиенты
// зависимостей читают актуальные значения на каждый запрос.
type Config struct {
	mu sync.RWMutex

	// Retry
	RetryEnabled  bool
	RetryMax      int           // число повторных попыток
	RetryBase     time.Duration // начальная задержка
	RetryMaxDelay time.Duration // потолок экспоненциальной задержки

	// Timeout
	TimeoutEnabled bool
	Timeout        time.Duration

	// Bulkhead
	BulkheadEnabled bool
	BulkheadLimit   int

	// Fallback
	FallbackEnabled bool
	FallbackTTL     time.Duration
}

// DefaultConfig возвращает конфигурацию по умолчанию. Паттерны выключены,
// чтобы никаких сюрпризов при старте стенда не было; включаются через админку.
func DefaultConfig() *Config {
	return &Config{
		RetryEnabled:    false,
		RetryMax:        2,
		RetryBase:       100 * time.Millisecond,
		RetryMaxDelay:   800 * time.Millisecond,
		TimeoutEnabled:  true,
		Timeout:         2 * time.Second,
		BulkheadEnabled: false,
		BulkheadLimit:   8,
		FallbackEnabled: false,
		FallbackTTL:     30 * time.Second,
	}
}

// Retry returns the retry config.
func (c *Config) Retry() RetryConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.RetryEnabled {
		return RetryConfig{}
	}
	return RetryConfig{MaxRetries: c.RetryMax, BaseDelay: c.RetryBase, MaxDelay: c.RetryMaxDelay}
}

// HasTimeout говорит, нужно ли оборачивать вызов в таймаут.
func (c *Config) HasTimeout() (bool, time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TimeoutEnabled && c.Timeout > 0, c.Timeout
}

// BulkheadSettings возвращает включать ли bulkhead и его лимит.
func (c *Config) BulkheadSettings() (bool, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BulkheadEnabled, c.BulkheadLimit
}

// FallbackSettings возвращает включать ли фолбэк и его TTL.
func (c *Config) FallbackSettings() (bool, time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.FallbackEnabled, c.FallbackTTL
}

// Set применяет частичное обновление настроек из внешнего DTO.
func (c *Config) Set(p Patch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.RetryEnabled != nil {
		c.RetryEnabled = *p.RetryEnabled
	}
	if p.RetryMax != nil {
		c.RetryMax = *p.RetryMax
	}
	if p.RetryBaseMs != nil {
		c.RetryBase = time.Duration(*p.RetryBaseMs) * time.Millisecond
	}
	if p.RetryMaxDelayMs != nil {
		c.RetryMaxDelay = time.Duration(*p.RetryMaxDelayMs) * time.Millisecond
	}
	if p.TimeoutEnabled != nil {
		c.TimeoutEnabled = *p.TimeoutEnabled
	}
	if p.TimeoutMs != nil {
		c.Timeout = time.Duration(*p.TimeoutMs) * time.Millisecond
	}
	if p.BulkheadEnabled != nil {
		c.BulkheadEnabled = *p.BulkheadEnabled
	}
	if p.BulkheadLimit != nil {
		c.BulkheadLimit = *p.BulkheadLimit
	}
	if p.FallbackEnabled != nil {
		c.FallbackEnabled = *p.FallbackEnabled
	}
	if p.FallbackTTLMs != nil {
		c.FallbackTTL = time.Duration(*p.FallbackTTLMs) * time.Millisecond
	}
}

// Snapshot — точка чтения/записи настроек одним куском (для админ-ручек).
type Snapshot struct {
	RetryEnabled    bool  `json:"retry_enabled"`
	RetryMax        int   `json:"retry_max"`
	RetryBaseMs     int64 `json:"retry_base_ms"`
	RetryMaxDelayMs int64 `json:"retry_max_delay_ms"`

	TimeoutEnabled bool  `json:"timeout_enabled"`
	TimeoutMs      int64 `json:"timeout_ms"`

	BulkheadEnabled bool `json:"bulkhead_enabled"`
	BulkheadLimit   int  `json:"bulkhead_limit"`

	FallbackEnabled bool  `json:"fallback_enabled"`
	FallbackTTLMs   int64 `json:"fallback_ttl_ms"`
}

// Snapshot возвращает копию текущих настроек.
func (c *Config) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{
		RetryEnabled:    c.RetryEnabled,
		RetryMax:        c.RetryMax,
		RetryBaseMs:     c.RetryBase.Milliseconds(),
		RetryMaxDelayMs: c.RetryMaxDelay.Milliseconds(),
		TimeoutEnabled:  c.TimeoutEnabled,
		TimeoutMs:       c.Timeout.Milliseconds(),
		BulkheadEnabled: c.BulkheadEnabled,
		BulkheadLimit:   c.BulkheadLimit,
		FallbackEnabled: c.FallbackEnabled,
		FallbackTTLMs:   c.FallbackTTL.Milliseconds(),
	}
}

// Patch — допустимые частичные обновления через REST-ручку. Каждое поле —
// указатель, чтобы можно было менять отдельные паттерны, не трогая остальные.
type Patch struct {
	RetryEnabled    *bool `json:"retry_enabled"`
	RetryMax        *int  `json:"retry_max"`
	RetryBaseMs     *int  `json:"retry_base_ms"`
	RetryMaxDelayMs *int  `json:"retry_max_delay_ms"`

	TimeoutEnabled *bool `json:"timeout_enabled"`
	TimeoutMs      *int  `json:"timeout_ms"`

	BulkheadEnabled *bool `json:"bulkhead_enabled"`
	BulkheadLimit   *int  `json:"bulkhead_limit"`

	FallbackEnabled *bool `json:"fallback_enabled"`
	FallbackTTLMs   *int  `json:"fallback_ttl_ms"`
}
