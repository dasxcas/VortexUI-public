-- +goose Up

-- Core binaries the panel knows about. One row per (core, version, arch,
-- source): "upstream" rows come from the vendor's releases and are not
-- necessarily meterable; "built" rows are artifacts this panel compiled itself
-- (sing-box with with_v2ray_api); "uploaded" rows are operator-supplied.
CREATE TABLE IF NOT EXISTS core_versions (
    id          UUID PRIMARY KEY,
    core        TEXT NOT NULL,              -- xray | singbox
    version     TEXT NOT NULL,
    arch        TEXT NOT NULL,              -- amd64 | arm64
    source      TEXT NOT NULL DEFAULT 'upstream',
    metered     BOOLEAN NOT NULL DEFAULT false,
    status      TEXT NOT NULL DEFAULT 'ready', -- ready | building | failed
    url         TEXT NOT NULL DEFAULT '',
    file        TEXT NOT NULL DEFAULT '',   -- artifact filename under the data dir
    sha256      TEXT NOT NULL DEFAULT '',
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (core, version, arch, source)
);

CREATE INDEX IF NOT EXISTS idx_core_versions_core ON core_versions (core, version);

-- +goose Down
DROP TABLE IF EXISTS core_versions;
