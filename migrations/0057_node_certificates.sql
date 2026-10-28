-- +goose Up

-- Per-node mTLS identities. Every node gets its own keypair issued by the
-- panel's CA, so a leaked node key identifies one node only and can be revoked
-- without rotating the whole fleet.
CREATE TABLE IF NOT EXISTS node_certificates (
    node_id     UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    serial      TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    cert_pem    TEXT NOT NULL,
    key_pem     TEXT NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_node_certificates_fingerprint
    ON node_certificates (fingerprint);

-- +goose Down
DROP TABLE IF EXISTS node_certificates;
