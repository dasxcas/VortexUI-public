-- +goose Up

-- Pending certificate delivered to a node but not yet promoted. Rotation arrives
-- here first: the panel pushes the new identity, and only once the agent confirms
-- the swap does it become the active certificate.
ALTER TABLE node_certificates ADD COLUMN IF NOT EXISTS pending_fingerprint TEXT;
ALTER TABLE node_certificates ADD COLUMN IF NOT EXISTS pending_cert_pem TEXT;
ALTER TABLE node_certificates ADD COLUMN IF NOT EXISTS pending_key_pem TEXT;

-- +goose Down
ALTER TABLE node_certificates DROP COLUMN IF EXISTS pending_fingerprint;
ALTER TABLE node_certificates DROP COLUMN IF EXISTS pending_cert_pem;
ALTER TABLE node_certificates DROP COLUMN IF EXISTS pending_key_pem;
