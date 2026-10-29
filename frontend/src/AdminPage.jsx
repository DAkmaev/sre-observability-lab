import { useCallback, useEffect, useState } from "react";
import {
  getPaymentChaos,
  getRecommendationsChaos,
  getRecommendationsPolicy,
  getResilience,
  setPaymentChaos,
  setRecommendationsChaos,
  setRecommendationsPolicy,
  setResilience as saveResilience,
  fetchStock,
  restock,
} from "./api.js";

const normalRecommendations = { fail_rate: 0, latency_ms: 0, rate_limit_rps: 0, rate_limit_burst: 1 };
const normalPayment = { fail_rate: 0, latency_ms: 0, stage: "bank", fail_kind: "card_declined" };
const normalResilience = {
  retry_enabled: false, retry_max: 2, retry_base_ms: 100, retry_max_delay_ms: 800,
  timeout_enabled: false, timeout_ms: 2000,
  bulkhead_enabled: false, bulkhead_limit: 8,
  fallback_enabled: false, fallback_ttl_ms: 30000,
};

function toNumber(value, fallback = 0) {
  const result = Number(value);
  return Number.isFinite(result) ? result : fallback;
}

function toBool(value, fallback = false) {
  if (value === true || value === "true" || value === "on") return true;
  if (value === false || value === "false" || value === "off") return false;
  return fallback;
}

function Status({ children }) {
  return <p className="admin-status" role="status">{children}</p>;
}

export default function AdminPage({ currentUser }) {
  const [policy, setPolicy] = useState("…");
  const [recommendations, setRecommendations] = useState(normalRecommendations);
  const [payment, setPayment] = useState(normalPayment);
  const [resilience, setResilience] = useState(normalResilience);
  const [message, setMessage] = useState("Загружаем текущую конфигурацию…");
  const [busy, setBusy] = useState(false);
  const [paymentOpen, setPaymentOpen] = useState(false);
  const [stock, setStock] = useState([]);
  const [stockMessage, setStockMessage] = useState("");
  const [stockBusy, setStockBusy] = useState(false);
  const [restockQty, setRestockQty] = useState({});

  const isAdmin = currentUser && currentUser.role === "admin";

  const refresh = useCallback(async () => {
    setBusy(true);
    try {
      const [nextPolicy, nextRecommendations, nextPayment, nextResilience] = await Promise.all([
        getRecommendationsPolicy(), getRecommendationsChaos(), getPaymentChaos(), getResilience(),
      ]);
      setPolicy(nextPolicy.mode);
      setRecommendations(nextRecommendations);
      setPayment(nextPayment);
      setResilience(nextResilience);
      setMessage("Конфигурация актуальна.");
    } catch (error) {
      setMessage(`Не удалось прочитать конфигурацию: ${error.message}`);
    } finally {
      setBusy(false);
    }
  }, []);

  const loadStock = useCallback(async () => {
    if (!isAdmin) return;
    setStockBusy(true);
    try {
      setStock(await fetchStock());
      setStockMessage("Остатки загружены.");
    } catch (error) {
      setStockMessage(`Не удалось загрузить остатки: ${error.message}`);
    } finally {
      setStockBusy(false);
    }
  }, [isAdmin]);

  useEffect(() => { refresh(); }, [refresh]);
  useEffect(() => { loadStock(); }, [loadStock]);

  async function doRestock(productId) {
    setStockBusy(true);
    try {
      const qty = Number(restockQty[productId] ?? 1) || 0;
      if (qty <= 0) {
        setStockMessage("Укажите положительное количество для restock.");
        setStockBusy(false);
        return;
      }
      await restock(productId, qty);
      await loadStock();
      setStockMessage(`Товар #${productId} восполнен на ${qty} шт.`);
    } catch (error) {
      setStockMessage(`Restock не применён: ${error.message}`);
      setStockBusy(false);
    }
  }

  async function run(action, successMessage) {
    setBusy(true);
    try {
      await action();
      await refresh();
      setMessage(successMessage);
    } catch (error) {
      setMessage(`Изменение не применено: ${error.message}`);
      setBusy(false);
    }
  }

  const setRecField = (field, value) => setRecommendations((current) => ({ ...current, [field]: value }));
  const setPaymentField = (field, value) => setPayment((current) => ({ ...current, [field]: value }));
  const setResField = (field, value) => setResilience((current) => ({ ...current, [field]: value }));

  return (
      <main className="admin-page">
        <header className="admin-header">
          <div>
            <p className="admin-kicker">SRE Observability Lab</p>
            <h1>Панель управления сценариями</h1>
            <p>Меняет конфигурацию живого стенда без curl и перезапуска контейнеров.</p>
          </div>
          <a className="admin-back" href="/">← К витрине</a>
        </header>

        <Status>{message}</Status>

        <section className="admin-grid">
          <article className="admin-card">
            <h2>Graceful degradation</h2>
            <p className="admin-help">Текущий режим: <strong>{policy}</strong></p>
            <div className="admin-actions">
              <button className={policy === "strict" ? "active" : ""} disabled={busy}
                      onClick={() => run(() => setRecommendationsPolicy("strict"), "Включён strict: recommendations обязательны.")}>Strict</button>
              <button className={policy === "graceful" ? "active" : ""} disabled={busy}
                      onClick={() => run(() => setRecommendationsPolicy("graceful"), "Включён graceful: карточки работают без рекомендаций.")}>Graceful</button>
            </div>
            <p className="admin-note"><strong>Strict:</strong> ошибка dependency → карточка 503/429. <strong>Graceful:</strong> карточка 200 без блока рекомендаций.</p>
          </article>

          <article className="admin-card">
            <h2>Быстрый сброс</h2>
            <p className="admin-help">Отключает хаос и rate limit, но оставляет выбранную политику graceful degradation.</p>
            <button className="danger-button" disabled={busy} onClick={() => run(
                () => Promise.all([setRecommendationsChaos(normalRecommendations), setPaymentChaos(normalPayment)]),
                "Хаос и rate limit выключены."
            )}>Вернуть сервисы к норме</button>
            <button className="text-button" disabled={busy} onClick={refresh}>↻ Обновить состояние</button>
          </article>

          <article className="admin-card admin-card-wide">
            <h2>Recommendations: отказ, задержка и rate limit</h2>
            <p className="admin-help">429 — защита от перегрузки, а не технический сбой: circuit breaker остаётся CLOSED.</p>
            <div className="admin-form">
              <label>Доля отказов <input type="number" min="0" max="1" step="0.1" value={recommendations.fail_rate}
                                         onChange={(e) => setRecField("fail_rate", e.target.value)} /></label>
              <label>Задержка, мс <input type="number" min="0" step="100" value={recommendations.latency_ms}
                                         onChange={(e) => setRecField("latency_ms", e.target.value)} /></label>
              <label>Лимит, запросов/с <input type="number" min="0" step="0.5" value={recommendations.rate_limit_rps}
                                              onChange={(e) => setRecField("rate_limit_rps", e.target.value)} /></label>
              <label>Burst <input type="number" min="1" step="1" value={recommendations.rate_limit_burst}
                                  onChange={(e) => setRecField("rate_limit_burst", e.target.value)} /></label>
            </div>
            <div className="admin-actions">
              <button disabled={busy} onClick={() => run(() => setRecommendationsChaos({
                fail_rate: toNumber(recommendations.fail_rate), latency_ms: toNumber(recommendations.latency_ms),
                rate_limit_rps: toNumber(recommendations.rate_limit_rps), rate_limit_burst: toNumber(recommendations.rate_limit_burst, 1),
              }), "Конфигурация recommendations применена.")}>Применить</button>
              <button disabled={busy} onClick={() => run(
                  () => setRecommendationsChaos({ ...normalRecommendations, rate_limit_rps: 1, rate_limit_burst: 1 }),
                  "Пресет '1 RPS' применён."
              )}>Пресет: 1 RPS</button>
              <button disabled={busy} onClick={() => run(
                  () => setRecommendationsChaos({ ...normalRecommendations, fail_rate: 1 }),
                  "Пресет 'outage' применён: рекомендации отдают 100% отказов."
              )}>Пресет: outage</button>
            </div>
          </article>

          <article className="admin-card admin-card-wide">
            <h2>Паттерны отказоустойчивости</h2>
            <p className="admin-help">Настраиваются на лету и действуют для обеих зависимостей (recommendations и payment).</p>
            <div className="admin-form admin-form-grid">
              <label className="toggle"><input type="checkbox" checked={resilience.retry_enabled}
                                               onChange={(e) => setResField("retry_enabled", e.target.checked)} /> Retry</label>
              <label>Попыток <input type="number" min="0" step="1" value={resilience.retry_max}
                                    onChange={(e) => setResField("retry_max", e.target.value)} /></label>
              <label>База, мс <input type="number" min="0" step="50" value={resilience.retry_base_ms}
                                     onChange={(e) => setResField("retry_base_ms", e.target.value)} /></label>

              <label className="toggle"><input type="checkbox" checked={resilience.timeout_enabled}
                                               onChange={(e) => setResField("timeout_enabled", e.target.checked)} /> Timeout</label>
              <label>Таймаут, мс <input type="number" min="0" step="100" value={resilience.timeout_ms}
                                        onChange={(e) => setResField("timeout_ms", e.target.value)} /></label>
              <span className="admin-help-inline">жёсткое ограничение времени на запрос к зависимости</span>

              <label className="toggle"><input type="checkbox" checked={resilience.bulkhead_enabled}
                                               onChange={(e) => setResField("bulkhead_enabled", e.target.checked)} /> Bulkhead</label>
              <label>Лимит <input type="number" min="1" step="1" value={resilience.bulkhead_limit}
                                  onChange={(e) => setResField("bulkhead_limit", e.target.value)} /></label>
              <span className="admin-help-inline">сколько одновременных запросов пускать к зависимости</span>

              <label className="toggle"><input type="checkbox" checked={resilience.fallback_enabled}
                                               onChange={(e) => setResField("fallback_enabled", e.target.checked)} /> Fallback</label>
              <label>TTL кэша, мс <input type="number" min="0" step="1000" value={resilience.fallback_ttl_ms}
                                         onChange={(e) => setResField("fallback_ttl_ms", e.target.value)} /></label>
              <span className="admin-help-inline">кэш последнего успеха, если зависимость упала</span>
            </div>
            <div className="admin-actions">
              <button disabled={busy} onClick={() => run(() => saveResilience({
                retry_enabled: toBool(resilience.retry_enabled),
                retry_max: toNumber(resilience.retry_max, 2),
                retry_base_ms: toNumber(resilience.retry_base_ms, 100),
                retry_max_delay_ms: toNumber(resilience.retry_max_delay_ms, 800),
                timeout_enabled: toBool(resilience.timeout_enabled, true),
                timeout_ms: toNumber(resilience.timeout_ms, 2000),
                bulkhead_enabled: toBool(resilience.bulkhead_enabled),
                bulkhead_limit: toNumber(resilience.bulkhead_limit, 8),
                fallback_enabled: toBool(resilience.fallback_enabled),
                fallback_ttl_ms: toNumber(resilience.fallback_ttl_ms, 30000),
              }), "Паттерны применены.")}>Применить</button>
              <button disabled={busy} onClick={() => run(
                  () => saveResilience({ ...normalResilience, retry_enabled: true, retry_max: 2, retry_base_ms: 100 }),
                  "Пресет 'retry' применён."
              )}>Пресет: retry</button>
              <button disabled={busy} onClick={() => run(
                  () => saveResilience({ ...normalResilience, timeout_enabled: true, timeout_ms: 300 }),
                  "Пресет 'жёсткий таймаут 300 мс' применён."
              )}>Пресет: жёсткий таймаут 300 мс</button>
              <button disabled={busy} onClick={() => run(
                  () => saveResilience({ ...normalResilience, bulkhead_enabled: true, bulkhead_limit: 1 }),
                  "Пресет 'bulkhead (1 слот)' применён."
              )}>Пресет: bulkhead (1 слот)</button>
              <button disabled={busy} onClick={() => run(
                  () => saveResilience({ ...normalResilience, fallback_enabled: true, fallback_ttl_ms: 30000 }),
                  "Пресет 'fallback' применён."
              )}>Пресет: fallback</button>
              <button disabled={busy} onClick={() => run(
                  () => saveResilience({ ...normalResilience }),
                  "Все паттерны выключены."
              )}>Выкл. все</button>
            </div>
          </article>

          <article className="admin-card admin-card-wide">
            <button className="admin-collapse-toggle" aria-expanded={paymentOpen}
                    onClick={() => setPaymentOpen((open) => !open)}>
              <span className={paymentOpen ? "admin-chevron open" : "admin-chevron"}>{paymentOpen ? "▼" : "▶"}</span>
              Настройки payment
            </button>
            {paymentOpen && (
                <div className="admin-collapse-body">
                  <div className="admin-form">
                    <label>Этап <select value={payment.stage} onChange={(e) => setPaymentField("stage", e.target.value)}>
                      <option value="bank">bank.authorize</option><option value="db">payments.persist</option>
                    </select></label>
                    <label>Причина <select value={payment.fail_kind} onChange={(e) => setPaymentField("fail_kind", e.target.value)}>
                      <option value="card_declined">card_declined</option><option value="issuer_timeout">issuer_timeout</option>
                      <option value="issuer_unavailable">issuer_unavailable</option><option value="db_unavailable">db_unavailable</option>
                    </select></label>
                    <label>Доля отказов <input type="number" min="0" max="1" step="0.1" value={payment.fail_rate}
                                               onChange={(e) => setPaymentField("fail_rate", e.target.value)} /></label>
                    <label>Задержка, мс <input type="number" min="0" step="100" value={payment.latency_ms}
                                               onChange={(e) => setPaymentField("latency_ms", e.target.value)} /></label>
                  </div>
                  <div className="admin-actions">
                    <button disabled={busy} onClick={() => run(() => setPaymentChaos({ ...payment, fail_rate: toNumber(payment.fail_rate), latency_ms: toNumber(payment.latency_ms) }), "Настройки payment применены.")}>Применить</button>
                    <button disabled={busy} onClick={() => run(
                        () => setPaymentChaos({ fail_rate: 0, latency_ms: 4000, stage: "bank", fail_kind: "issuer_timeout" }),
                        "Пресет 'таймаут банка' применён."
                    )}>Пресет: таймаут банка</button>
                    <button disabled={busy} onClick={() => run(
                        () => setPaymentChaos({ fail_rate: 0.6, latency_ms: 0, stage: "bank", fail_kind: "card_declined" }),
                        "Пресет 'отказ карт' применён."
                    )}>Пресет: отказ карт</button>
                    <button disabled={busy} onClick={() => run(
                        () => setPaymentChaos({ fail_rate: 0.3, latency_ms: 3000, stage: "db", fail_kind: "db_unavailable" }),
                        "Пресет 'БД payment' применён."
                    )}>Пресет: БД payment</button>
                  </div>
                </div>
            )}
          </article>

          {isAdmin && (
            <article className="admin-card admin-card-wide">
              <h2>📦 Склад (остатки)</h2>
              <p className="admin-help">Текущие остатки по товарам. Если товара нет в наличии, покупка даёт 409. «Restock» возвращает позицию на склад.</p>
              {stockMessage && <Status>{stockMessage}</Status>}
              <table className="admin-stock-table">
                <thead>
                  <tr><th>#</th><th>Товар</th><th>Остаток</th><th>Статус</th><th>Restock</th></tr>
                </thead>
                <tbody>
                  {stock.map((item) => (
                    <tr key={item.product_id}>
                      <td>{item.product_id}</td>
                      <td>{item.emoji} {item.name}</td>
                      <td>{item.stock}</td>
                      <td>{item.in_stock ? "В наличии" : "Нет"}</td>
                      <td>
                        <div className="admin-restock">
                          <input
                            type="number"
                            min="1"
                            value={restockQty[item.product_id] ?? 1}
                            onChange={(e) => setRestockQty((q) => ({ ...q, [item.product_id]: e.target.value }))}
                          />
                          <button disabled={stockBusy} onClick={() => doRestock(item.product_id)}>+ пополнить</button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <div className="admin-actions">
                <button className="text-button" disabled={stockBusy} onClick={loadStock}>↻ Обновить остатки</button>
              </div>
            </article>
          )}
        </section>
      </main>
  );
}
