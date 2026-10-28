-- +goose Up

-- 0060: single-use enrollment links
--
-- The node installer is a curl|bash one-liner, so the credential in that URL is
-- the enrollment itself: one node, one use, short life. Only a hash is stored so
-- a database leak cannot be replayed against /join.
CREATE TABLE IF NOT EXISTS enrollment_links (
    id         UUID PRIMARY KEY,
    node_id    UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_enrollment_links_node ON enrollment_links(node_id);
