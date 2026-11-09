CREATE TABLE IF NOT EXISTS endpoints (
    id                      TEXT PRIMARY KEY,
    port                    INTEGER NOT NULL UNIQUE CHECK (port BETWEEN 1 AND 65535),
    allow_management        BOOLEAN NOT NULL,
    allow_proxy             BOOLEAN NOT NULL,
    require_proxy_auth_info BOOLEAN NOT NULL DEFAULT false,
    allow_http_forward      BOOLEAN NOT NULL,
    allow_http_reverse      BOOLEAN NOT NULL,
    allow_socks5            BOOLEAN NOT NULL,
    created_at_ns           BIGINT NOT NULL,
    updated_at_ns           BIGINT NOT NULL
);
