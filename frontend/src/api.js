// Простой клиент к backend API. Все запросы идут через gateway по
// относительному пути /api/... — сам gateway (nginx) проксирует их на
// backend и проставляет X-Request-ID, если его ещё нет.

// ── Сессия (модульный singleton) ─────────────────────────────────────────
// Текущий пользователь живёт здесь, чтобы и App.jsx, и AdminPage.jsx могли
// его читать. Фронт хранит токен в localStorage; каждый запрос шлёт
// X-User-Name + X-Session-Token, а backend сверяет их со своей in-memory
// сессией и не даёт притвориться админом одним заголовком.

let session = {
  token: null,
  name: null,
  role: "guest",
};

// Восстанавливаем токен и имя из localStorage при загрузке (роль отдельно
// не храним — после перезагрузки фронт сверяется с backend через
// GET /api/session; X-User-Name обязателен, т.к. backend сверяет сессию по
// связке token+name).
const savedToken = localStorage.getItem("sre_store_token");
const savedName = localStorage.getItem("sre_store_user");
if (savedToken) session.token = savedToken;
if (savedName) session.name = savedName;

export function setSession(next) {
  session = { ...session, ...next };
  if (session.token) {
    localStorage.setItem("sre_store_token", session.token);
  } else {
    localStorage.removeItem("sre_store_token");
  }
  if (session.name) {
    localStorage.setItem("sre_store_user", session.name);
  } else {
    localStorage.removeItem("sre_store_user");
  }
}

export function getSession() {
  return session;
}

function userHeaders(headers) {
  const h = new Headers(headers);
  if (session.token) h.set("X-Session-Token", session.token);
  if (session.name) h.set("X-User-Name", session.name);
  return h;
}

// Создаёт и экспортирует корневой W3C Trace Context для одного
// пользовательского действия. Backend извлекает traceparent через
// OpenTelemetry и продолжает этот trace во всех последующих сервисах.
function createTraceContext() {
  const bytes = new Uint8Array(24); // 16 байт trace ID + 8 байт span ID
  crypto.getRandomValues(bytes);

  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return {
    traceId: hex.slice(0, 32),
    spanId: hex.slice(32),
    startTime: BigInt(Date.now()) * 1_000_000n,
  };
}

function exportRootSpan(context, path, method, status, error) {
  const attributes = [
    { key: "http.request.method", value: { stringValue: method } },
    { key: "url.path", value: { stringValue: path } },
  ];
  if (status) attributes.push({ key: "http.response.status_code", value: { intValue: String(status) } });
  if (error) attributes.push({ key: "error.type", value: { stringValue: error.name || "Error" } });

  // Tempo принимает OTLP/HTTP. Запрос отправляется через gateway, поэтому
  // браузеру не нужны отдельный порт Tempo или CORS-настройка.
  void fetch("/v1/traces", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    keepalive: true,
    body: JSON.stringify({
      resourceSpans: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "frontend" } }] },
        scopeSpans: [{
          scope: { name: "sre-store-frontend" },
          spans: [{
            traceId: context.traceId,
            spanId: context.spanId,
            name: `${method} ${path}`,
            kind: 3, // SPAN_KIND_CLIENT: браузер — клиент, вызывающий backend
            startTimeUnixNano: context.startTime.toString(),
            endTimeUnixNano: (BigInt(Date.now()) * 1_000_000n).toString(),
            attributes,
            status: error ? { code: 2, message: error.message } : { code: 1 },
          }],
        }],
      }],
    }),
  }).catch(() => {
    // Экспорт наблюдаемости не должен влиять на пользовательский запрос.
  });
}

async function apiFetch(path, options = {}) {
  const context = createTraceContext();
  const headers = userHeaders(options.headers);
  headers.set("traceparent", `00-${context.traceId}-${context.spanId}-01`);

  try {
    const response = await fetch(path, { ...options, headers });
    exportRootSpan(context, path, options.method || "GET", response.status);
    return response;
  } catch (error) {
    exportRootSpan(context, path, options.method || "GET", undefined, error);
    throw error;
  }
}

// Учебная админ-панель работает через gateway: браузеру не нужен доступ к
// внутренним портам backend/recommendations/payment. Сюда тоже пробрасываем
// user-заголовки — backend сверяет их сессией и даёт доступ только admin.
async function adminFetch(path, options = {}) {
  const response = await apiFetch(`/lab-admin/${path}`, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  if (!response.ok) {
    throw new Error(`${options.method || "GET"} /lab-admin/${path} -> ${response.status}`);
  }
  return response.json();
}

export const getRecommendationsPolicy = () => adminFetch("recommendations-policy");
export const setRecommendationsPolicy = (mode) =>
  adminFetch("recommendations-policy", { method: "POST", body: JSON.stringify({ mode }) });

export const getRecommendationsChaos = () => adminFetch("recommendations-chaos");
export const setRecommendationsChaos = (config) =>
  adminFetch("recommendations-chaos", { method: "POST", body: JSON.stringify(config) });

export const getPaymentChaos = () => adminFetch("payment-chaos");
export const setPaymentChaos = (config) =>
  adminFetch("payment-chaos", { method: "POST", body: JSON.stringify(config) });

export const getResilience = () => adminFetch("resilience");
export const setResilience = (config) =>
  adminFetch("resilience", { method: "POST", body: JSON.stringify(config) });

export async function fetchProducts() {
  const res = await apiFetch("/api/products");
  if (!res.ok) throw new Error(`GET /api/products -> ${res.status}`);
  return res.json();
}

export async function fetchProduct(id) {
  const res = await apiFetch(`/api/products/${id}`);
  if (!res.ok) throw new Error(`GET /api/products/${id} -> ${res.status}`);
  return res.json();
}

// checkout отправляет содержимое корзины на бэкенд (POST /api/orders).
// Бэкенд сам берёт актуальные цены из БД, оплата проходит через
// payment-сервис. Ответ приходит и при успехе, и при отказе оплаты
// (HTTP 402) — в обоих случаях это валидный JSON с заказом, поэтому
// ошибкой (throw) считаем только реальные технические сбои (5xx/сеть).
// HTTP 409 — отдельный случай: заказ НЕ создаётся (не хватает остатков),
// оплата вообще не инициировалась.
export async function checkout(items) {
  const res = await apiFetch("/api/orders", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ items }),
  });
  if (res.status === 401) {
    return { ok: false, reason: "not_logged_in", order: undefined };
  }
  if (res.status >= 500) {
    throw new Error(`POST /api/orders -> ${res.status}`);
  }
  if (res.status === 409) {
    return { ok: false, reason: "out_of_stock", order: undefined };
  }
  const data = await res.json();
  return { ok: res.ok, reason: res.ok ? "paid" : "payment_failed", order: data.order };
}

// ── Сессии ────────────────────────────────────────────────────────────────

export async function fetchSession() {
  const res = await apiFetch("/api/session");
  if (!res.ok) throw new Error(`GET /api/session -> ${res.status}`);
  return res.json();
}

export async function login(name) {
  const res = await apiFetch("/api/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw new Error(`POST /api/session -> ${res.status}`);
  return res.json();
}

export async function logout() {
  try {
    await apiFetch("/api/session", { method: "DELETE" });
  } finally {
    setSession({ token: null, name: null, role: "guest" });
  }
}

// ── Отзывы и рейтинги ─────────────────────────────────────────────────────

export async function fetchReviews(productId) {
  const res = await apiFetch(`/api/products/${productId}/reviews`);
  if (!res.ok) throw new Error(`GET /api/products/${productId}/reviews -> ${res.status}`);
  return res.json();
}

export async function postReview({ product_id, rating, comment }) {
  const res = await apiFetch("/api/reviews", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ product_id, rating, comment }),
  });
  if (res.status === 401) {
    throw new Error("unauthorized");
  }
  if (!res.ok) throw new Error(`POST /api/reviews -> ${res.status}`);
  return res.json();
}

// ── Склад (админка) ───────────────────────────────────────────────────────

export const fetchStock = () => adminFetch("stock");

export function restock(productId, qty) {
  return adminFetch(`products/${productId}/restock`, {
    method: "POST",
    body: JSON.stringify({ qty }),
  });
}

export function formatPrice(cents) {
  return (cents / 100).toLocaleString("ru-RU", {
    style: "currency",
    currency: "RUB",
    maximumFractionDigits: 0,
  });
}

export async function fetchOrders() {
  const res = await apiFetch("/api/orders");
  if (!res.ok) throw new Error(`GET /api/orders -> ${res.status}`);
  return res.json();
}

export function describeFailureReason(reason) {
  switch (reason) {
    case "card_declined":
      return "карта отклонена банком";
    case "issuer_timeout":
    case "issuer_unavailable":
    case "payment_service_unavailable":
      return "сервис оплаты временно недоступен, попробуйте позже";
    case "db_unavailable":
      return "техническая ошибка при обработке платежа";
    default:
      return reason || "неизвестная причина";
  }
}
