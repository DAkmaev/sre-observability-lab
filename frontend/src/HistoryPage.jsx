import { useEffect, useState, useCallback } from "react";
import { fetchOrders, formatPrice, describeFailureReason } from "./api.js";

function OrderCard({ order, showBuyer }) {
  const isPaid = order.status === "paid";
  const date = new Date(order.created_at);
  const formattedDate = date.toLocaleString("ru-RU", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });

  return (
    <div className="order-card">
      <div className="order-header">
        <div>
          <span className="order-id">Заказ #{order.id}</span>
          <span className="order-date">{formattedDate}</span>
          {showBuyer && (
            <span className="order-buyer">👤 {order.user_name || `#${order.user_id}`}</span>
          )}
        </div>
        <span className={`order-status ${isPaid ? "order-status-paid" : "order-status-failed"}`}>
          {isPaid ? "✅ Оплачено" : "❌ Оплата не прошла"}
        </span>
      </div>

      {!isPaid && (
        <div className="order-failure">
          Причина: {describeFailureReason(order.failure_reason || "")}
          {order.payment_id && (
            <span className="order-payment-id"> (payment: {order.payment_id})</span>
          )}
        </div>
      )}

      <div className="order-total">
        Итого: {formatPrice(order.total_cents)}
      </div>

      <ul className="order-items">
        {order.items.map((item, idx) => (
          <li key={idx} className="order-item">
            <span className="order-item-name">{item.name}</span>
            <span className="order-item-qty">×{item.quantity}</span>
            <span className="order-item-subtotal">{formatPrice(item.price_cents * item.quantity)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export default function HistoryPage({ currentUser }) {
  const [orders, setOrders] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const isLoggedIn = currentUser && currentUser.logged;
  const isAdmin = isLoggedIn && currentUser.role === "admin";

  const load = useCallback(() => {
    setLoading(true);
    setError(null);
    fetchOrders()
      .then((data) => {
        setOrders(Array.isArray(data) ? data : []);
        setLoading(false);
      })
      .catch((e) => {
        setError(e.message);
        setLoading(false);
      });
  }, []);

  useEffect(() => {
    if (isLoggedIn) {
      load();
    } else {
      setLoading(false);
    }
  }, [load, isLoggedIn]);

  if (!isLoggedIn) {
    return (
      <div className="history-page">
        <header className="history-header">
          <h2 className="history-title">📋 История покупок</h2>
          <a href="/" className="history-nav">← На витрину</a>
        </header>
        <p className="history-login-required">
          История покупок доступна только залогиненным пользователям.{" "}
          <a href="/">Войдите на витрине</a> и откройте историю ещё раз.
        </p>
      </div>
    );
  }

  return (
    <div className="history-page">
      <header className="history-header">
        <h2 className="history-title">📋 История покупок</h2>
        <a href="/" className="history-nav">← На витрину</a>
      </header>

      {loading && <p className="history-loading">Загрузка заказов…</p>}

      {error && (
        <div className="history-error">
          Не удалось загрузить историю: {error}{" "}
          <button onClick={load}>Повторить</button>
        </div>
      )}

      {!loading && !error && orders.length === 0 && (
        <p className="history-empty">Покупок пока нет</p>
      )}

      {!loading && !error && orders.length > 0 && (
        <div className="history-list">
          {orders.map((order) => (
            <OrderCard key={order.id} order={order} showBuyer={isAdmin} />
          ))}
        </div>
      )}
    </div>
  );
}
