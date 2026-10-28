package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
)

// NodeJoin turns enrollment into one command the operator can paste on a new
// machine: the panel serves a script that installs the agent, its certificate
// bundle and a systemd unit.
//
// The link is the credential. It is single-use, bound to one node, short-lived,
// and stored only as a hash — a leaked database row cannot be replayed.
type NodeJoin struct {
	repo      port.EnrollmentLinkRepository
	nodes     nodeGetter
	certs     NodeCertIssuer
	bundles   bundleForNode
	publicURL string
	agentDir  string
	log       *slog.Logger
	ttl       time.Duration
	now       func() time.Time
}

// NodeCertIssuer issues (or reuses) one node's identity.
type NodeCertIssuer interface {
	Ensure(ctx context.Context, node *domain.Node) (*domain.NodeCertificate, error)
}

// bundleForNode renders the ca/node/key tarball for a node.
type bundleForNode interface {
	BundleFor(cert *domain.NodeCertificate) (*EnrollmentBundle, error)
}

// nodeGetter is the subset of the node service this needs.
type nodeGetter interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.Node, error)
}

// JoinLink is what the API returns to the console.
type JoinLink struct {
	Token     string    `json:"token"`
	Command   string    `json:"command"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewNodeJoin wires the join service. agentDir holds the agent binary that is
// served to nodes; without it the script falls back to a clear error.
func NewNodeJoin(repo port.EnrollmentLinkRepository, nodes nodeGetter, certs NodeCertIssuer,
	bundles bundleForNode, publicURL, agentDir string, log *slog.Logger) *NodeJoin {
	return &NodeJoin{
		repo: repo, nodes: nodes, certs: certs, bundles: bundles,
		publicURL: strings.TrimRight(publicURL, "/"), agentDir: agentDir,
		log: log, ttl: 10 * time.Minute, now: time.Now,
	}
}

// SetTTL overrides the link lifetime (used by tests).
func (j *NodeJoin) SetTTL(d time.Duration) { j.ttl = d }

// Create mints a link for one node and returns the command that redeems it.
func (j *NodeJoin) Create(ctx context.Context, nodeID uuid.UUID) (*JoinLink, error) {
	if j.publicURL == "" {
		return nil, errors.New("VORTEX_PUBLIC_URL is not set; the join command needs a URL the node can reach")
	}
	node, err := j.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	// Issue the identity now: the script embeds it, so the node never has to
	// call back for credentials before it can authenticate.
	cert, err := j.certs.Ensure(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("issue node certificate: %w", err)
	}
	if cert == nil || len(cert.CertPEM) == 0 {
		return nil, errors.New("node certificate was not issued")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	link := &domain.EnrollmentLink{
		ID:        uuid.New(),
		NodeID:    nodeID,
		TokenHash: hashToken(token),
		ExpiresAt: j.now().Add(j.ttl),
		CreatedAt: j.now(),
	}
	if err := j.repo.Create(ctx, link); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("curl -fsSL %s/join/%s | sudo bash", j.publicURL, token)
	j.log.Info("join link created", "node", node.Name, "expires_at", link.ExpiresAt)
	return &JoinLink{Token: token, Command: cmd, ExpiresAt: link.ExpiresAt}, nil
}

// Redeem consumes a token and returns the installer script. The token is spent
// even if the caller never runs the script: a fetched link that stayed valid
// would be a second credential sitting in shell history.
func (j *NodeJoin) Redeem(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", domain.ErrNotFound
	}
	link, err := j.repo.Redeem(ctx, hashToken(token))
	if err != nil {
		return "", err
	}
	node, err := j.nodes.Get(ctx, link.NodeID)
	if err != nil {
		return "", err
	}
	cert, err := j.certs.Ensure(ctx, node)
	if err != nil {
		return "", err
	}
	bundle, err := j.bundles.BundleFor(cert)
	if err != nil {
		return "", err
	}
	script := renderJoinScript(joinScriptData{
		PanelURL: j.publicURL,
		NodeName: node.Name,
		NodeAddr: node.Address,
		Bundle:   bundle.Bundle,
		AgentURL: j.agentURL(),
	})
	j.log.Info("join link redeemed", "node", node.Name)
	return script, nil
}

// RevokeAll expires every unused link for a node, so a command that leaked can be
// made useless without waiting for the TTL.
func (j *NodeJoin) RevokeAll(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	n, err := j.repo.RevokeByNode(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	j.log.Info("join links revoked", "node", nodeID, "count", n)
	return n, nil
}

// agentURL points at the binary this panel ships.
func (j *NodeJoin) agentURL() string {
	if j.agentDir == "" {
		return ""
	}
	return j.publicURL + "/downloads/agent/vortex-node"
}

// AgentPath returns the on-disk agent binary, for the download handler.
func (j *NodeJoin) AgentPath() string {
	if j.agentDir == "" {
		return ""
	}
	return filepath.Join(j.agentDir, "vortex-node")
}

// AgentAvailable reports whether this image carries the agent at all.
func (j *NodeJoin) AgentAvailable() bool {
	p := j.AgentPath()
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() > 0
}

type joinScriptData struct {
	PanelURL string
	NodeName string
	NodeAddr string
	Bundle   string
	AgentURL string
}

// joinScript is intentionally small and defensive: it runs as root on a machine
// the panel has never seen, so every step either succeeds or stops.
const joinScript = `#!/usr/bin/env bash
# VortexUI node installer — generated for one node, single use.
set -euo pipefail

PANEL="__PANEL__"
NODE_NAME="__NODE_NAME__"
LISTEN="0.0.0.0:50051"
AGENT_URL="__AGENT_URL__"
BUNDLE="__BUNDLE__"
CERT_DIR="/etc/vortex/certs"
ENV_FILE="/etc/vortex/node.env"
BIN="/usr/local/bin/vortex-node"
SERVICE="vortexui-node"

say()  { printf '  %s\n' "$1"; }
fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "run as root (pipe into sudo bash)"

say "installing agent for node \"$NODE_NAME\""
if [ -z "$AGENT_URL" ]; then
  fail "this panel does not ship an agent binary; build one with: CGO_ENABLED=0 go build -o vortex-node ./cmd/node"
fi
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar  >/dev/null 2>&1 || fail "tar is required"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --max-time 300 -o "$tmp/vortex-node" "$AGENT_URL" || fail "cannot download the agent from $PANEL"
[ "$(head -c 4 "$tmp/vortex-node" | od -An -tx1 | tr -d ' \n')" = "7f454c46" ] || fail "downloaded agent is not an ELF binary"
install -m 0755 "$tmp/vortex-node" "$BIN"
say "agent installed at $BIN"

mkdir -p "$CERT_DIR" && chmod 700 "$CERT_DIR"
printf '%s' "$BUNDLE" | base64 -d > "$tmp/bundle.tar" || fail "cannot decode the certificate bundle"
tar -x -C "$CERT_DIR" -f "$tmp/bundle.tar" || fail "cannot unpack the certificate bundle"
chmod 600 "$CERT_DIR"/*.key
say "certificates installed in $CERT_DIR"

cat > "$ENV_FILE" <<EOF
VORTEX_NODE_LISTEN=$LISTEN
VORTEX_TLS_CERT=$CERT_DIR/node.crt
VORTEX_TLS_KEY=$CERT_DIR/node.key
VORTEX_TLS_CA=$CERT_DIR/ca.crt
VORTEX_CORE=xray
VORTEX_ENABLED_CORES=xray
VORTEX_CORE_BIN=/usr/local/bin/xray
VORTEX_CORE_CONFIG=/etc/vortex/xray.json
VORTEX_CORE_API_PORT=10085
EOF
chmod 600 "$ENV_FILE"
say "env written to $ENV_FILE"

if [ -d /run/systemd/system ]; then
  cat > "/etc/systemd/system/$SERVICE.service" <<EOF
[Unit]
Description=VortexUI Node Agent
After=network.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$SERVICE" >/dev/null 2>&1 || true
  systemctl restart "$SERVICE"
  sleep 2
  if systemctl is-active --quiet "$SERVICE"; then
    say "service $SERVICE is running"
  else
    journalctl -u "$SERVICE" -n 20 --no-pager || true
    fail "service did not stay up (see the log above)"
  fi
else
  say "no systemd here — start it yourself: $BIN"
fi

echo
say "done. The node should appear as connected in $PANEL within a few seconds."
say "firewall: allow 50051/tcp from the panel only."
`

func renderJoinScript(d joinScriptData) string {
	r := strings.NewReplacer(
		"__PANEL__", d.PanelURL,
		"__NODE_NAME__", escapeShell(d.NodeName),
		"__AGENT_URL__", d.AgentURL,
		"__BUNDLE__", d.Bundle,
	)
	return r.Replace(joinScript)
}

// escapeShell keeps a node name from breaking out of the double-quoted literal.
func escapeShell(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s)
}
