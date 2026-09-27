import { useEffect, useState, useCallback, useMemo } from "react";
import {
  fetchProducts,
  fetchProduct,
  checkout,
  formatPrice,
  describeFailureReason,
  fetchSession,
  login,
  logout,
  setSession,
  getSession,
  fetchReviews,
  postReview,
} from "./api.js";
import HistoryPage from "./HistoryPage.jsx";
import AdminPage from "./AdminPage.jsx";

function ProductCard({ product, onOpen, onAddToCart }) {
  return (
    <div className="card">
      <button className="card-body" onClick={() => onOpen(product.id)}>
        <div className="card-emoji">{product.emoji}</div>
        <div className="card-name">{product.name}</div>
        <div className="card-price">{formatPrice(product.price_cents)}</div>
        {product.in_stock ? (
          <div className="card-stock">В наличии: {product.stock} шт.</div>
        ) : (
          <div className="card-stock out">Нет в наличии</div>
        )}
      </button>
      <button
        className="card-add-btn"
        disabled={!product.in_stock}
        onClick={(e) => {
          e.stopPropagation();
          onAddToCart(product);
        }}
      >
        + В корзину
      </button>
    </div>
  );
}

function RecommendedList({ items }) {
  return (
    <ul className="rec-list">
      {items.map((r) => (
        <li key={r.product_id}>
          товар #{r.product_id} · релевантность {Math.round(r.score * 100)}%
        </li>
      ))}
    </ul>
  );
}

// Список отзывов + средний рейтинг товара (в модалке).
function ReviewsBlock({ id, detail, currentUser, onReviewPosted }) {
  const [reviews, setReviews] = useState([]);
  const [error, setError] = useState(null);
  const [rating, setRating] = useState(5);
  const [comment, setComment] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState(null);

  const load = useCallback(() => {
    setError(null);
    fetchReviews(id)
      .then(setReviews)
      .catch((e) => setError(e.message));
  }, [id]);

  useEffect(() => {
    load();
  }, [load, detail]);

  async function handleSubmit(e) {
    e.preventDefault();
    if (!currentUser || !currentUser.logged) {
      setSubmitError("Войдите, чтобы оставить отзыв.");
      return;
    }
    setSubmitting(true);
    setSubmitError(null);
    try {
      await postReview({ product_id: id, rating, comment });
      setComment("");
      setRating(5);
      load();
      onReviewPosted && onReviewPosted();
    } catch (err) {
      setSubmitError(err.message === "unauthorized" ? "Войдите, чтобы оставить отзыв." : err.message);
    } finally {
      setSubmitting(false);
    }
  }

  const stars = Array.from({ length: 5 }, (_, i) => i + 1);

  return (
    <div className="reviews-block">
      <h3>
        ⭐ Отзывы{" "}
        {detail && detail.rating_count > 0 && (
          <span className="reviews-avg">
            ({detail.avg_rating.toFixed(1)} из 5 · {detail.rating_count} оцен.)
          </span>
        )}
      </h3>

      {error && <p className="error">Не удалось загрузить отзывы: {error}</p>}
      {!error && reviews.length === 0 && <p className="reviews-empty">Отзывов пока нет.</p>}

      {reviews.length > 0 && (
        <ul className="reviews-list">
          {reviews.map((r) => (
            <li key={r.id} className="review-item">
              <div className="review-meta">
                <span className="review-user">{r.user_name || `#${r.user_id}`}</span>
                <span className="review-stars">{"★".repeat(r.rating)}</span>
                <span className="review-date">{new Date(r.created_at).toLocaleString("ru-RU")}</span>
              </div>
              <div className="review-comment">{r.comment}</div>
            </li>
          ))}
        </ul>
      )}

      <form className="review-form" onSubmit={handleSubmit}>
        <div className="review-stars-input">
          <span className="review-stars-label">Оценка:</span>
          {stars.map((s) => (
            <button
              key={s}
              type="button"
              className={`review-star ${s <= rating ? "on" : ""}`}
              onClick={() => setRating(s)}
              aria-label={`Оценка ${s}`}
            >
              ★
            </button>
          ))}
        </div>
        <textarea
          rows="2"
          placeholder="Ваш комментарий…"
          value={comment}
          onChange={(e) => setComment(e.target.value)}
        />
        <div className="review-form-actions">
          <button type="submit" disabled={submitting}>
            {submitting ? "Отправляем…" : "Оставить отзыв"}
          </button>
        </div>
        {submitError && <p className="error">{submitError}</p>}
      </form>
    </div>
  );
}

function ProductDetail({ id, onClose, onAddToCart, currentUser, onReviewPosted }) {
  const [detail, setDetail] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(null);
    fetchProduct(id)
      .then((d) => !cancelled && setDetail(d))
      .catch((e) => !cancelled && setError(e.message));
    return () => {
      cancelled = true;
    };
  }, [id]);

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <button className="modal-close" onClick={onClose}>
          ✕
        </button>

        {error && <p className="error">Не удалось загрузить товар: {error}</p>}

        {!detail && !error && <p>Загрузка…</p>}

        {detail && (
          <>
            <div className="modal-emoji">{detail.emoji}</div>
            <h2>{detail.name}</h2>
            <p className="modal-price">{formatPrice(detail.price_cents)}</p>
            <p className="modal-desc">{detail.description}</p>

            <p className={detail.in_stock ? "stock-info" : "stock-info out"}>
              {detail.in_stock ? `В наличии: ${detail.stock} шт.` : "Нет в наличии"}
            </p>

            <button
              className="modal-add-btn"
              disabled={!detail.in_stock}
              onClick={() => {
                onAddToCart(detail);
                onClose();
              }}
            >
              + В корзину
            </button>

            <div className="rec-block">
              <h3>Рекомендуем также</h3>
              {detail.recommendations_available ? (
                <>
                  <p className="popularity">
                    Индекс популярности: {detail.popularity_score.toFixed(1)}
                  </p>
                  <RecommendedList items={detail.recommended} />
                </>
              ) : (
                <p className="rec-unavailable">
                  Рекомендации временно недоступны — попробуйте позже.
                </p>
              )}
            </div>

            <ReviewsBlock id={id} detail={detail} currentUser={currentUser} onReviewPosted={onReviewPosted} />
          </>
        )}
      </div>
    </div>
  );
}

// ── Корзина и оформление заказа ──────────────────────────────────────────

function CartDrawer({ cart, onClose, onChangeQty, onRemove, onCheckout, checkoutState }) {
  const total = cart.reduce((sum, i) => sum + i.price_cents * i.quantity, 0);

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal cart-modal" onClick={(e) => e.stopPropagation()}>
        <button className="modal-close" onClick={onClose}>
          ✕
        </button>
        <h2>🛒 Корзина</h2>

        {cart.length === 0 && <p className="cart-empty">Корзина пуста</p>}

        {cart.length > 0 && (
          <>
            <ul className="cart-list">
              {cart.map((item) => (
                <li key={item.product_id} className="cart-item">
                  <span className="cart-item-emoji">{item.emoji}</span>
                  <div className="cart-item-info">
                    <div className="cart-item-name">{item.name}</div>
                    <div className="cart-item-price">{formatPrice(item.price_cents)}</div>
                  </div>
                  <div className="cart-item-qty">
                    <button onClick={() => onChangeQty(item.product_id, item.quantity - 1)}>−</button>
                    <span>{item.quantity}</span>
                    <button onClick={() => onChangeQty(item.product_id, item.quantity + 1)}>+</button>
                  </div>
                  <button className="cart-item-remove" onClick={() => onRemove(item.product_id)}>
                    ✕
                  </button>
                </li>
              ))}
            </ul>

            <div className="cart-total">Итого: {formatPrice(total)}</div>

            <button
              className="checkout-btn"
              disabled={checkoutState.status === "loading"}
              onClick={onCheckout}
            >
              {checkoutState.status === "loading" ? "Оформляем…" : "Оформить заказ"}
            </button>

            {checkoutState.status === "success" && (
              <p className="checkout-success">
                ✅ Заказ #{checkoutState.order.id} оплачен, спасибо!
              </p>
            )}
            {checkoutState.status === "not_logged_in" && (
              <p className="checkout-failed">
                ⚠️ Чтобы оформить заказ, <strong>войдите</strong> (нужно для
                ведения истории покупок).
              </p>
            )}
            {checkoutState.status === "out_of_stock" && (
              <p className="checkout-failed">
                ❌ Не хватает остатков на складе: заказ не создан. Восполните остатки и повторите.
              </p>
            )}
            {checkoutState.status === "failed" && (
              <p className="checkout-failed">
                ❌ Заказ #{checkoutState.order && checkoutState.order.id ? checkoutState.order.id : ""} создан, но оплата не прошла:{" "}
                {describeFailureReason(checkoutState.order && checkoutState.order.failure_reason)}
              </p>
            )}
            {checkoutState.status === "error" && (
              <p className="checkout-failed">
                ⚠️ Не удалось оформить заказ: {checkoutState.message}
              </p>
            )}
          </>
        )}
      </div>
    </div>
  );
}

function LoginBox({ currentUser, onLogin, onLogout }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  async function handleLogin(e) {
    e.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await onLogin(name.trim());
      setName("");
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  if (currentUser.logged) {
    return (
      <div className="login-box">
        <span className="login-name">
          👤 {currentUser.name}
          {currentUser.role === "admin" && <span className="login-role"> (admin)</span>}
        </span>
        <button className="login-logout" onClick={onLogout}>Выйти</button>
      </div>
    );
  }

  return (
    <form className="login-box" onSubmit={handleLogin}>
      <input
        className="login-input"
        placeholder="Ваше имя"
        value={name}
        onChange={(e) => setName(e.target.value)}
      />
      <button className="login-submit" disabled={busy || !name.trim()}>
        {busy ? "…" : "Войти"}
      </button>
      {error && <span className="login-error">{error}</span>}
    </form>
  );
}

export default function App() {
  // Сначала объявляем ВСЕ хуки, потом ветвление по роуту — иначе React
  // жалуется на "Rendered fewer hooks than expected" при переходах между
  // страницами (админка/история возвращаются раньше объявления остальных хуков).
  const [products, setProducts] = useState([]);
  const [loadError, setLoadError] = useState(null);
  const [openId, setOpenId] = useState(null);
  const [cartOpen, setCartOpen] = useState(false);
  const [cart, setCart] = useState([]);
  const [checkoutState, setCheckoutState] = useState({ status: "idle" });
  const [currentUser, setCurrentUser] = useState(() => ({
    ...getSession(),
    logged: !!getSession().token,
  }));

  const path = window.location.pathname;

  const load = useCallback(() => {
    setLoadError(null);
    fetchProducts()
      .then(setProducts)
      .catch((e) => setLoadError(e.message));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Восстанавливаем сессию: фронт помнит токен, но роль/имена — только у backend.
  useEffect(() => {
    fetchSession()
      .then((data) => {
        if (data.logged) {
          setSession({ name: data.name, role: data.role });
          setCurrentUser({ name: data.name, role: data.role, logged: true });
        } else {
          // Backend не знает наш токен (например, его перезапустили, и
          // in-memory сессии обнулились). Сбрасываем устаревший токен и
          // состояние в guest, иначе фронт остаётся «залогиненным» полу-состоянием
          // (logged=true, но role/name пустые) и ссылка на админку пропадает.
          setSession({ token: null, name: null, role: "guest" });
          setCurrentUser({ name: null, role: "guest", logged: false });
        }
      })
      .catch(() => {
        setSession({ token: null, name: null, role: "guest" });
        setCurrentUser({ name: null, role: "guest", logged: false });
      });
  }, []);

  const cartCount = useMemo(
    () => cart.reduce((sum, i) => sum + i.quantity, 0),
    [cart]
  );

  const addToCart = useCallback((product) => {
    setCheckoutState({ status: "idle" });
    setCart((prev) => {
      const existing = prev.find((i) => i.product_id === product.id);
      if (existing) {
        return prev.map((i) =>
          i.product_id === product.id ? { ...i, quantity: i.quantity + 1 } : i
        );
      }
      return [
        ...prev,
        {
          product_id: product.id,
          name: product.name,
          emoji: product.emoji,
          price_cents: product.price_cents,
          quantity: 1,
        },
      ];
    });
  }, []);

  const changeQty = useCallback((productId, quantity) => {
    setCart((prev) => {
      if (quantity <= 0) {
        return prev.filter((i) => i.product_id !== productId);
      }
      return prev.map((i) => (i.product_id === productId ? { ...i, quantity } : i));
    });
  }, []);

  const removeFromCart = useCallback((productId) => {
    setCart((prev) => prev.filter((i) => i.product_id !== productId));
  }, []);

  const handleCheckout = useCallback(async () => {
    setCheckoutState({ status: "loading" });
    try {
      const { ok, order, reason } = await checkout(
        cart.map((i) => ({ product_id: i.product_id, quantity: i.quantity }))
      );
      if (ok) {
        setCheckoutState({ status: "success", order });
        setCart([]);
        // Покупка списала остатки на складе — обновляем витрину, чтобы
        // счётчики "В наличии: N шт." не оставались устаревшими до рефреша.
        load();
      } else if (reason === "out_of_stock") {
        setCheckoutState({ status: "out_of_stock" });
      } else if (reason === "not_logged_in") {
        setCheckoutState({ status: "not_logged_in" });
      } else {
        setCheckoutState({ status: "failed", order });
      }
    } catch (e) {
      setCheckoutState({ status: "error", message: e.message });
    }
  }, [cart, load]);

  const handleLogin = useCallback(async (name) => {
    const data = await login(name);
    setSession({ token: data.token, name: data.name, role: data.role });
    setCurrentUser({ name: data.name, role: data.role, logged: true });
  }, []);

  const handleLogout = useCallback(async () => {
    await logout();
    setCurrentUser({ name: null, role: "guest", logged: false });
  }, []);

  const refreshProducts = useCallback(() => load(), [load]);

  if (path === "/admin" || path === "/admin/") {
    return <AdminPage key={currentUser.role} currentUser={currentUser} />;
  }
  if (path === "/history" || path === "/history/") {
    return <HistoryPage currentUser={currentUser} />;
  }

  return (
    <div className="app">
      <header className="header">
        <div className="header-top">
          <div className="header-title">
            <h1>🛠️ SRE Store</h1>
            <p className="tagline">Мерч для тех, кто дежурит по расписанию</p>
          </div>
          <div className="header-actions">
            <LoginBox currentUser={currentUser} onLogin={handleLogin} onLogout={handleLogout} />
            <button className="cart-btn" onClick={() => setCartOpen(true)} title="Корзина">
              🛒{cartCount > 0 && <span className="cart-badge">{cartCount}</span>}
            </button>
          </div>
        </div>
        <nav className="header-nav">
          {currentUser.logged && currentUser.role === "admin" && (
            <a className="store-admin-link" href="/admin/">⚙ Управление лабой</a>
          )}
          <a className="store-admin-link store-history-link" href="/history/">📋 История покупок</a>
        </nav>
      </header>

      {loadError && (
        <div className="banner-error">
          Не удалось загрузить витрину: {loadError}{" "}
          <button onClick={load}>Повторить</button>
        </div>
      )}

      <main className="grid">
        {products.map((p) => (
          <ProductCard key={p.id} product={p} onOpen={setOpenId} onAddToCart={addToCart} />
        ))}
      </main>

      {openId !== null && (
        <ProductDetail
          id={openId}
          onClose={() => setOpenId(null)}
          onAddToCart={addToCart}
          currentUser={currentUser}
          onReviewPosted={refreshProducts}
        />
      )}

      {cartOpen && (
        <CartDrawer
          cart={cart}
          onClose={() => setCartOpen(false)}
          onChangeQty={changeQty}
          onRemove={removeFromCart}
          onCheckout={handleCheckout}
          checkoutState={checkoutState}
        />
      )}
    </div>
  );
}