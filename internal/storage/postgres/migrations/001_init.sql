-- Начальная схема сервиса сокращения ссылок.

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE links (
    id           BIGSERIAL PRIMARY KEY,
    code         VARCHAR(32) NOT NULL UNIQUE,
    original_url TEXT NOT NULL,
    owner_id     BIGINT REFERENCES users(id) ON DELETE CASCADE,
    title        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    is_active    BOOLEAN NOT NULL DEFAULT true,
    click_count  BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX idx_links_owner_created ON links (owner_id, created_at DESC);

CREATE TABLE clicks (
    id         BIGSERIAL PRIMARY KEY,
    link_id    BIGINT NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    clicked_at TIMESTAMPTZ NOT NULL,
    referrer   TEXT NOT NULL DEFAULT '',
    country    CHAR(2) NOT NULL DEFAULT 'ZZ',
    browser    TEXT NOT NULL DEFAULT '',
    os         TEXT NOT NULL DEFAULT '',
    device     TEXT NOT NULL DEFAULT '',
    visitor_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_clicks_link_time ON clicks (link_id, clicked_at);
