-- Схема и seed-данные для витрины "SRE Store" —
-- тематический магазин мерча для SRE-инженеров.

CREATE TABLE IF NOT EXISTS products (
    id          SERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    price_cents INTEGER NOT NULL,
    emoji       TEXT NOT NULL DEFAULT '📦',
    category    TEXT NOT NULL DEFAULT 'misc',
    stock       INTEGER NOT NULL DEFAULT 0
);

-- У большинства товаров есть остаток, у пары — 0, чтобы на витрине можно
-- было показать состояние "нет в наличии" и 409 при попытке покупки.
INSERT INTO products (name, description, price_cents, emoji, category, stock) VALUES
('Error Budget Jar', 'Стеклянная банка с делениями "потрачено / осталось". Наполняется тревогой каждый инцидент.', 129900, '🫙', 'desk', 25),
('Toil Repellent Spray', 'Аэрозоль от рутины. При регулярном применении заменяется автоматизацией.', 89900, '🧴', 'desk', 0),
('99.999% Uptime Mug', 'Кружка, которая почти никогда не бьётся. Почти.', 79900, '☕', 'desk', 40),
('Blameless Postmortem Journal', 'Блокнот для разбора инцидентов без поиска виноватых. 200 страниц системного мышления.', 149900, '📓', 'desk', 15),
('Chaos Monkey Plush', 'Мягкая игрушка, которая случайным образом падает с полки посреди ночи.', 199900, '🐒', 'toys', 10),
('kubectl Cheat Sheet Poster', 'Постер А2 с самыми нужными командами kubectl. Проверено — спасает на собеседованиях.', 59900, '🗺️', 'poster', 0),
('On-Call Survival Kit', 'Энергетик, беруши и стикер "Не будите, если не Critical". Для ночных дежурств.', 249900, '🎒', 'kit', 8),
('SLO Dashboard Sticker Pack', '12 стикеров с любимыми PromQL-запросами на ноутбук.', 39900, '🏷️', 'stickers', 60),
('Vintage Pager Replica', 'Точная копия пейджера 2003 года — года основания SRE в Google. Не звонит, только вибрирует от испуга.', 179900, '📟', 'desk', 12),
('Canary Deployment Hoodie', 'Жёлтая толстовка. Выкатывается сначала на 5% гардероба, потом на все 100%.', 349900, '🟡', 'apparel', 20),
('Rubber Duck Debugger', 'Резиновая уточка для объяснения бага вслух. Уровень доверия — production.', 69900, '🦆', 'toys', 30),
('Circuit Breaker Keychain', 'Брелок с переключателем: closed / open / half-open. Щёлкает очень удовлетворительно.', 49900, '🔌', 'stickers', 45);

-- ── Пользователи и сессии ────────────────────────────────────────────────
-- Лёгкая аутентификация для лабы: вход по имени без пароля. Роль 'admin'
-- даёт доступ к админ-панели; остальным — только чтение и отзывы.
-- Идентичность персистится, чтобы отзывы и целостность данных пережили
-- рестарт backend (сами сессии живут только в памяти backend).
-- Должны создаваться ДО orders/reviews, потому что те ссылаются на users(id).

CREATE TABLE IF NOT EXISTS users (
    id         SERIAL PRIMARY KEY,
    name       TEXT UNIQUE NOT NULL,
    role       TEXT NOT NULL DEFAULT 'user',   -- user | admin
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO users (name, role) VALUES ('admin', 'admin')
ON CONFLICT (name) DO NOTHING;

-- ── Заказы (добавлено вместе с "Добавить в корзину" / "Купить") ──────────
-- Корзина живёт на фронтенде (в state React, без персистентности) —
-- это учебный стенд, а не production e-commerce. А вот заказ, который
-- получился в результате оформления корзины, уже сохраняется здесь —
-- именно по нему потом идёт трейс backend → payment.

CREATE TABLE IF NOT EXISTS orders (
    id            SERIAL PRIMARY KEY,
    -- Кто купил. NOT NULL в свежей схеме: заказ в этой версии создаётся только
    -- авторизованным пользователем (checkout требует входа). При миграции на
    -- живую БД колонку добавляют пустой, поэтому код умеет и NULL.
    user_id       INTEGER NOT NULL REFERENCES users(id),
    status        TEXT NOT NULL,              -- paid | failed
    total_cents   INTEGER NOT NULL,
    payment_id    TEXT,                        -- id платежа в payment-сервисе (если был создан)
    failure_reason TEXT,                        -- причина отказа, если status = failed
    request_id    TEXT,                         -- correlation ID запроса оформления заказа
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Быстрый поиск истории по покупателю (админ видит всех).
CREATE INDEX IF NOT EXISTS idx_orders_user_id ON orders(user_id);

CREATE TABLE IF NOT EXISTS order_items (
    id          SERIAL PRIMARY KEY,
    order_id    INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id  INTEGER NOT NULL REFERENCES products(id),
    quantity    INTEGER NOT NULL,
    price_cents INTEGER NOT NULL               -- цена на момент покупки (снапшот)
);

-- ── Платежи (пишет payment-сервис) ────────────────────────────────────────
-- Отдельная таблица, а не общая с orders, потому что в реальной системе
-- это была бы отдельная БД отдельного сервиса — здесь используем ту же
-- Postgres ради простоты стенда, но с чётким разделением "кто пишет".

CREATE TABLE IF NOT EXISTS payments (
    id          TEXT PRIMARY KEY,              -- pay_<random>
    order_ref   TEXT NOT NULL,                 -- произвольный внешний идентификатор заказа
    amount_cents INTEGER NOT NULL,
    status      TEXT NOT NULL,                 -- approved | declined | error
    reason      TEXT,                           -- issuer_declined | issuer_timeout | db_unavailable | ok
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Отзывы и рейтинги ─────────────────────────────────────────────────────
-- Второй "пишущий" путь после checkout: отдельные span'ы, validation-ошибки
-- (рейтинг вне 1..5, пустой комментарий) дают инциденты для наблюдения.

CREATE TABLE IF NOT EXISTS reviews (
    id         SERIAL PRIMARY KEY,
    product_id INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    user_id    INTEGER REFERENCES users(id),
    rating     INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_reviews_product ON reviews(product_id);

-- ── Склад: ledger остатков ────────────────────────────────────────────────
-- Каждое изменение stock (списание в checkout, компенсирующее возвращение
-- при неудачной оплате, восполнение через админку) пишется сюда. Это и
-- "реалистичность", и дополнительный источник наблюдаемости.

CREATE TABLE IF NOT EXISTS stock_movements (
    id           SERIAL PRIMARY KEY,
    product_id   INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    qty_delta    INTEGER NOT NULL,             -- +увеличили / -списали
    before_stock INTEGER NOT NULL,
    after_stock  INTEGER NOT NULL,
    reason       TEXT NOT NULL,                -- reserve | restock | unreserve
    request_id   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_stock_movements_product ON stock_movements(product_id);
