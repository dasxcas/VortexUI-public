#!/usr/bin/env bash
# VortexUI panel bootstrap — one command from a fresh clone to a working console.
#
#   sudo ./scripts/bootstrap.sh --domain panel.example.com --email you@example.com
#
# What it does, in order, and only what is missing:
#   1. checks root, OS, and that the ports the stack needs are free
#   2. installs Docker (official convenience script) when absent
#   3. writes a private mTLS chain: ca + panel leaf + node leaf
#   4. writes .env with random secrets, keeping any that already exist
#   5. builds and starts the stack as compose project "vortexui"
#   6. waits for /api/health
#   7. creates the first admin and writes its password to a 0600 file
#   8. prints the login URL, the subscription base and the one backup warning
#
# Re-running is safe: existing secrets, certificates and the admin are kept.
set -euo pipefail

# ---- defaults ---------------------------------------------------------------
DOMAIN=""
EMAIL=""
ADDRESS=""
DIR=""
ADMIN_USER="root"
ADMIN_PASSWORD=""
SKIP_BUILD=0
REBUILD=0
ASSUME_YES=0
HTTPS_ONLY=0
PROJECT="vortexui"
HEALTH_TIMEOUT=240

# ---- output -----------------------------------------------------------------
if [[ -t 1 ]]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_BLU=$'\033[34m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_DIM=""; C_OFF=""
fi
step() { printf '%s==>%s %s\n' "$C_BLU" "$C_OFF" "$1"; }
ok()   { printf '%s  ok%s %s\n' "$C_GRN" "$C_OFF" "$1"; }
warn() { printf '%swarn%s %s\n' "$C_YEL" "$C_OFF" "$1"; }
die()  { printf '%sfail%s %s\n' "$C_RED" "$C_OFF" "$1" >&2; exit 1; }
dim()  { printf '%s%s%s\n' "$C_DIM" "$1" "$C_OFF"; }

usage() {
  cat <<'EOF'
Usage: sudo ./scripts/bootstrap.sh [options]

  --domain NAME       public hostname; enables automatic HTTPS via Caddy
  --email ADDR        ACME contact address (used with --domain)
  --https-only        refuse to continue without a domain (never serve plain HTTP)
  --address ADDR      Caddy listen address when no domain is given (default ":80")
  --dir PATH          install directory (default: this checkout, else /opt/vortexui)
  --admin-user NAME   first admin username (default: root)
  --admin-password P  first admin password (default: generated, written to disk)
  --skip-build        start existing images without rebuilding
  --rebuild           rebuild even when this checkout was already built
  --yes               do not prompt (only affects the Docker install prompt)
  -h, --help          this text

Without --domain (and without --yes) the script asks for one on a terminal.
Answering empty keeps the old behaviour: plain HTTP on --address, fine for a
private network but the subscription URL is then http://, not https://.
With --https-only an empty answer is an error instead.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    --address) ADDRESS="${2:-}"; shift 2 ;;
    --dir) DIR="${2:-}"; shift 2 ;;
    --admin-user) ADMIN_USER="${2:-}"; shift 2 ;;
    --admin-password) ADMIN_PASSWORD="${2:-}"; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --rebuild) REBUILD=1; shift ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    --https-only|--no-http) HTTPS_ONLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

# ---- 1. preconditions -------------------------------------------------------
step "checking this machine"
[[ $EUID -eq 0 ]] || die "run as root (sudo ./scripts/bootstrap.sh ...)"
[[ -f /etc/os-release ]] || die "cannot identify the OS (/etc/os-release missing)"
. /etc/os-release
case "${ID:-}" in
  debian|ubuntu) ok "os: ${PRETTY_NAME:-$ID}" ;;
  *) warn "os ${PRETTY_NAME:-$ID} is untested; Debian/Ubuntu is what this script was written for" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  CORE_ARCH=amd64 ;;
  aarch64|arm64) CORE_ARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
ok "architecture: $ARCH (core artifacts: $CORE_ARCH)"

# Resolve the source tree: this script's parent when it is the repo, else --dir.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_FROM_SCRIPT="$(cd "$SCRIPT_DIR/.." && pwd)"
if [[ -z "$DIR" ]]; then
  if [[ -f "$REPO_FROM_SCRIPT/go.mod" && -f "$REPO_FROM_SCRIPT/deploy/compose.yml" ]]; then
    DIR="$REPO_FROM_SCRIPT"
  else
    DIR="/opt/vortexui"
  fi
fi
[[ -f "$DIR/go.mod" ]] || die "no checkout at $DIR (clone the repo there, or pass --dir)"
[[ -f "$DIR/deploy/compose.yml" ]] || die "$DIR is not a VortexUI checkout (deploy/compose.yml missing)"
cd "$DIR"
ok "source: $DIR"

# Ask for a hostname on a terminal. `curl | bash` means stdin is the script, so
# prompts read /dev/tty; without a terminal we fall back to the old behaviour.
ask() {
  local prompt="$1" answer=""
  if [[ -r /dev/tty ]]; then
    read -r -p "$prompt" answer < /dev/tty || true
  fi
  printf '%s' "$answer"
}

if [[ -z "$DOMAIN" && $HTTPS_ONLY -eq 0 && $ASSUME_YES -eq 0 && -r /dev/tty ]]; then
  step "public address"
  dim "  https needs a hostname whose A record already points at this machine."
  dim "  press enter twice to keep it simple: IP + http on port 80 (not recommended)."
  DOMAIN="$(ask "panel domain (e.g. panel.example.com): ")"
  DOMAIN="$(printf '%s' "$DOMAIN" | tr -d '[:space:]' | sed 's|^https\?://||; s|/$||')"
fi

if [[ -n "$DOMAIN" ]]; then
  if [[ ! "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$ ]]; then
    die "\"$DOMAIN\" does not look like a hostname (want panel.example.com, not an IP or a URL)"
  fi
  if [[ -z "$EMAIL" && $ASSUME_YES -eq 0 && -r /dev/tty ]]; then
    EMAIL="$(ask "email for certificate expiry notices (optional): ")"
  fi
  SITE_ADDRESS="$DOMAIN"
  PUBLIC_URL="https://$DOMAIN"
  ok "domain: $DOMAIN (Caddy will obtain and renew the certificate)"
else
  if [[ $HTTPS_ONLY -eq 1 ]]; then
    die "https-only was requested but no domain was given: pass --domain panel.example.com"
  fi
  SITE_ADDRESS="${ADDRESS:-:80}"
  PUBLIC_URL=""
  warn "no domain: the panel will be served on ${SITE_ADDRESS} over HTTP"
fi

# If this project is already running, it owns ports 8080/50051 and re-running
# must continue rather than refuse.
OWN_RUNNING=0
if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "${PROJECT}-panel-1"; then
  OWN_RUNNING=1
fi

need_port() {
  local port="$1" label="$2"
  if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"; then
    die "$label port $port is already in use — stop that service or move this stack"
  fi
}
if [[ $OWN_RUNNING -eq 1 ]]; then
  ok "this stack is already running (ports belong to it)"
else
  need_port 8080 "panel HTTP"
  need_port 50051 "node gRPC"
  if [[ -z "$DOMAIN" && "$SITE_ADDRESS" == ":80" ]]; then
    need_port 80 "web"
  fi
  if [[ -n "$DOMAIN" ]]; then
    need_port 80 "ACME HTTP"
    need_port 443 "HTTPS"
  fi
  ok "required ports are free"
fi

# A wrong or proxied A record is the most common reason ACME never finishes, and
# Caddy only says "no certificate" 90 seconds later, so check it up front.
if [[ -n "$DOMAIN" ]]; then
  step "checking dns"
  WANT_IP="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  GOT_IPS="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')"
  if [[ -z "$GOT_IPS" ]]; then
    warn "$DOMAIN does not resolve yet — add an A record, then re-run (or wait for propagation)"
  elif [[ -n "$WANT_IP" && " $GOT_IPS " != *" $WANT_IP "* ]]; then
    warn "$DOMAIN resolves to ${GOT_IPS% } but this host is $WANT_IP"
    dim "  If that address is a CDN/proxy, turn the proxy off (grey cloud) or"
    dim "  ACME will fail; if the record is simply old, wait for the TTL."
  else
    ok "$DOMAIN -> ${GOT_IPS% }"
  fi
fi

# ---- 2. docker --------------------------------------------------------------
step "docker"
if ! command -v docker >/dev/null 2>&1; then
  warn "docker is not installed"
  if [[ $ASSUME_YES -ne 1 ]]; then
    read -r -p "Install Docker from get.docker.com now? [Y/n] " reply
    case "${reply:-Y}" in [yY]*) ;; *) die "docker is required; install it and re-run" ;; esac
  fi
  curl -fsSL https://get.docker.com -o /tmp/get-docker.sh || die "cannot download the Docker installer"
  sh /tmp/get-docker.sh || die "docker installation failed"
  rm -f /tmp/get-docker.sh
fi
docker info >/dev/null 2>&1 || die "dockerd is not running (try: systemctl enable --now docker)"
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is missing (install docker-compose-plugin)"
ok "docker $(docker version --format '{{.Server.Version}}'), compose $(docker compose version --short)"

# ---- 3. certificates --------------------------------------------------------
step "mTLS chain"
CERT_DIR="$DIR/deploy/certs"
if [[ -f "$CERT_DIR/ca.crt" && -f "$CERT_DIR/ca.key" && -f "$CERT_DIR/panel.crt" && -f "$CERT_DIR/node.crt" ]]; then
  ok "certificates already present (left untouched)"
else
  command -v openssl >/dev/null 2>&1 || die "openssl is required to mint the mTLS chain"
  mkdir -p "$CERT_DIR"
  chmod 700 "$CERT_DIR"

  # CA — 10 years. This key signs every node identity; it stays on this host.
  openssl ecparam -name prime256v1 -genkey -noout -out "$CERT_DIR/ca.key" 2>/dev/null
  openssl req -new -x509 -key "$CERT_DIR/ca.key" -sha256 -days 3650 \
    -subj "/CN=VortexUI Local CA" -out "$CERT_DIR/ca.crt"

  leaf() { # name, CN, EKU, SAN
    local name="$1" cn="$2" eku="$3" san="$4"
    openssl ecparam -name prime256v1 -genkey -noout -out "$CERT_DIR/$name.key" 2>/dev/null
    openssl req -new -key "$CERT_DIR/$name.key" -subj "/CN=$cn" -out "$CERT_DIR/$name.csr"
    cat > "$CERT_DIR/$name.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=$eku
subjectAltName=$san
EOF
    openssl x509 -req -in "$CERT_DIR/$name.csr" -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" \
      -CAcreateserial -days 3650 -sha256 -extfile "$CERT_DIR/$name.ext" -out "$CERT_DIR/$name.crt" 2>/dev/null
    rm -f "$CERT_DIR/$name.csr" "$CERT_DIR/$name.ext"
  }

  # The panel is the TLS *client* toward nodes, each node agent the server. They
  # must never share a keypair: one leaked identity would impersonate the other.
  leaf panel "panel" "clientAuth" "DNS:panel.local,DNS:localhost,IP:127.0.0.1"
  leaf node "node" "serverAuth" "DNS:localhost,DNS:node.local,IP:127.0.0.1"

  chmod 600 "$CERT_DIR"/*.key
  chmod 644 "$CERT_DIR"/*.crt
  ok "generated ca, panel and node certificates"
fi

# ---- 4. env -----------------------------------------------------------------
step "environment"
ENV_FILE="$DIR/.env"
rand() { openssl rand -hex "${1:-16}"; }

# Never rotate a secret that is already in use: it logs everyone out and breaks
# the database login.
get_env() { [[ -f "$ENV_FILE" ]] && sed -n "s/^$1=//p" "$ENV_FILE" | head -1 || true; }

DB_PASSWORD="$(get_env DB_PASSWORD)"; [[ -n "$DB_PASSWORD" ]] || DB_PASSWORD="$(rand 16)"
JWT_SECRET="$(get_env JWT_SECRET)";   [[ -n "$JWT_SECRET" ]] || JWT_SECRET="$(rand 32)"
INIT_TOKEN="$(get_env INIT_TOKEN)";   [[ -n "$INIT_TOKEN" ]] || INIT_TOKEN="$(rand 16)"
[[ -n "$PUBLIC_URL" ]] || PUBLIC_URL="$(get_env PUBLIC_URL)"
if [[ -z "$PUBLIC_URL" ]]; then
  IP="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')"
  PORT_SUFFIX=""
  # ":8080" in SITE_ADDRESS means the port has to survive into the public URL,
  # otherwise subscription links point at port 80 where nothing is listening.
  if [[ "$SITE_ADDRESS" == :* ]]; then PORT_SUFFIX="$SITE_ADDRESS"; fi
  PUBLIC_URL="http://${IP:-127.0.0.1}${PORT_SUFFIX}"
fi

cat > "$ENV_FILE" <<EOF
# Generated by scripts/bootstrap.sh — keep this file out of version control.
DB_PASSWORD=$DB_PASSWORD
JWT_SECRET=$JWT_SECRET
INIT_TOKEN=$INIT_TOKEN
PUBLIC_URL=$PUBLIC_URL
SITE_ADDRESS=$SITE_ADDRESS
ACME_EMAIL=$EMAIL
PERSONAL_MODE=true
CORE=xray
ENABLED_CORES=xray
# Uncomment to compile new metered sing-box versions on demand (needs Docker):
# VORTEX_CORE_BUILD=true
EOF
chmod 600 "$ENV_FILE"
ok "wrote $ENV_FILE (existing secrets preserved)"

# ---- 5. build and start -----------------------------------------------------
step "starting the stack"
# The .env lives next to the checkout, but the compose file is in deploy/, and
# compose only auto-loads a .env from the project directory — so pass it
# explicitly or every ${JWT_SECRET} interpolation fails.
compose() {
  docker compose -p "$PROJECT" --env-file "$DIR/.env" -f "$DIR/deploy/compose.yml" "$@"
}

compose config >/dev/null 2>&1 || {
  docker compose -p "$PROJECT" --env-file "$DIR/.env" -f "$DIR/deploy/compose.yml" config >/tmp/vortex-config.log 2>&1 || true
  tail -10 /tmp/vortex-config.log
  die "compose configuration is invalid (see above)"
}

build_stack() {
  if [[ $SKIP_BUILD -eq 1 ]]; then
    dim "  --skip-build: using existing images"
    return 0
  fi
  # Skip a rebuild when the same checkout was already built: a re-run should be
  # instant, and nothing changed.
  local stamp_file="$DIR/.bootstrap-stamp"
  local rev
  rev="$(git -C "$DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
  if [[ $REBUILD -eq 0 && -f "$stamp_file" && "$(cat "$stamp_file")" == "$rev" ]] \
     && docker image inspect "${PROJECT}-panel:latest" >/dev/null 2>&1 \
     && docker image inspect "${PROJECT}-web:latest" >/dev/null 2>&1; then
    dim "  images already built for $rev (use --rebuild to force)"
    SKIP_BUILD=1
    return 0
  fi

  dim "  building images (first run pulls the base images; a few minutes)"
  if compose build >/tmp/vortex-build.log 2>&1; then
    echo "$rev" > "$stamp_file"
    return 0
  fi
  # A host whose Docker bridge has no DNS fails while downloading the core
  # archives, and the error does not always say "dns" — retry unconditionally
  # with the host network, which is harmless when the cause was something else.
  warn "compose build failed; retrying with --network host"
  docker build --network host -f "$DIR/deploy/Dockerfile" --target panel-aio \
      -t "${PROJECT}-panel:latest" "$DIR" >>/tmp/vortex-build.log 2>&1 \
    || { tail -20 /tmp/vortex-build.log; die "panel image build failed"; }
  docker build --network host -f "$DIR/deploy/web.Dockerfile" \
      -t "${PROJECT}-web:latest" "$DIR" >>/tmp/vortex-build.log 2>&1 \
    || { tail -20 /tmp/vortex-build.log; die "web image build failed"; }
  ok "images built with the host network"
  echo "$rev" > "$stamp_file"
  SKIP_BUILD=1
  return 0
}

build_stack
if [[ $SKIP_BUILD -eq 1 ]]; then
  compose up -d --no-build || die "compose up failed (see: docker compose -p $PROJECT logs)"
else
  compose up -d || die "compose up failed (see: docker compose -p $PROJECT logs)"
fi
ok "containers started"

# ---- 6. health --------------------------------------------------------------
step "waiting for the panel"
deadline=$((SECONDS + HEALTH_TIMEOUT))
until curl -fsS --max-time 3 http://127.0.0.1:8080/api/health >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    compose ps || true
    compose logs --tail 30 panel || true
    die "the panel did not become healthy within ${HEALTH_TIMEOUT}s"
  fi
  sleep 3
done
ok "panel is healthy at http://127.0.0.1:8080"

# The container is healthy long before ACME finishes; wait for the public URL so
# the operator does not open https:// and see a handshake error.
if [[ -n "$DOMAIN" ]]; then
  step "waiting for the certificate"
  dim "  first issuance usually takes 10-30s; Caddy renews it from then on"
  TLS_DEADLINE=$((SECONDS + 120))
  while ! curl -fsS --max-time 4 "https://$DOMAIN/api/health" >/dev/null 2>&1; do
    if (( SECONDS > TLS_DEADLINE )); then
      warn "https://$DOMAIN is still not answering"
      dim "  Caddy keeps retrying. check: docker compose -p $PROJECT logs --tail 30 caddy"
      dim "  usual causes: 80/443 blocked upstream, DNS not pointing here, CDN proxy on,"
      dim "  or rate limits after repeated failed attempts (wait an hour and re-run)."
      break
    fi
    sleep 3
  done
  if curl -fsS --max-time 4 "https://$DOMAIN/api/health" >/dev/null 2>&1; then
    ok "https://$DOMAIN is live"
  fi
fi

# ---- 7. admin ---------------------------------------------------------------
step "first admin"
CRED_FILE="$DIR/admin-credentials.txt"
if [[ -z "$ADMIN_PASSWORD" ]]; then
  ADMIN_PASSWORD="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-18)"
fi
if compose exec -T panel panel admin create --username "$ADMIN_USER" --password "$ADMIN_PASSWORD" --sudo >/tmp/vortex-admin.log 2>&1; then
  umask 077
  cat > "$CRED_FILE" <<EOF
username: $ADMIN_USER
password: $ADMIN_PASSWORD
created:  $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
  chmod 600 "$CRED_FILE"
  ok "admin \"$ADMIN_USER\" created (password in $CRED_FILE)"
else
  if grep -qiE 'already exists|duplicate' /tmp/vortex-admin.log; then
    warn "an admin already exists; password left unchanged"
  else
    tail -5 /tmp/vortex-admin.log
    warn "could not create the admin automatically — run:"
    dim "  docker compose -p $PROJECT -f deploy/compose.yml exec panel panel admin create --username $ADMIN_USER --password '<choose>' --sudo"
  fi
fi

# ---- 8. summary -------------------------------------------------------------
echo
printf '%s────────────────────────────────────────────%s\n' "$C_GRN" "$C_OFF"
echo " VortexUI is up"
printf '%s────────────────────────────────────────────%s\n' "$C_GRN" "$C_OFF"
echo "  console      $PUBLIC_URL"
echo "  subscription $PUBLIC_URL/sub/<token>"
echo "  project      docker compose -p $PROJECT -f deploy/compose.yml ..."
echo "  logs         docker compose -p $PROJECT logs -f panel"
echo
if [[ -n "$DOMAIN" ]]; then
  echo "  certificate  Let's Encrypt via Caddy (auto-renew; http redirects to https)"
fi
echo "  node address to enter in the UI: <this-host>:50051"
echo

if [[ ! -f "$CRED_FILE" ]]; then
  echo "  admin        $ADMIN_USER (password unchanged, not re-printed)"
else
  echo "  admin        $ADMIN_USER — password once in $CRED_FILE"
fi

echo
warn "back up deploy/certs/ca.key offline."
dim "  It signs every node identity. Lose it and every node must re-enroll;"
dim "  leak it and an attacker can mint a node certificate."
echo
dim "Next: Nodes → New, then install the agent with the bundle it gives you."
