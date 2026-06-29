#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/ssw-cloud/v2sudoku.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"
REPO_SLUG="${REPO_SLUG:-ssw-cloud/v2sudoku}"
INSTALL_DIR="${INSTALL_DIR:-/opt/v2sudoku}"
SRC_DIR="${SRC_DIR:-${INSTALL_DIR}/src}"
BIN_PATH="${BIN_PATH:-${INSTALL_DIR}/v2sudoku}"
CONFIG_DIR="${CONFIG_DIR:-/etc/v2sudoku}"
CONFIG_PATH="${CONFIG_PATH:-${CONFIG_DIR}/config.yml}"
SERVICE_NAME="${SERVICE_NAME:-v2sudoku}"
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
LOG_DIR="${LOG_DIR:-/var/log/v2sudoku}"
LOGROTATE_PATH="/etc/logrotate.d/${SERVICE_NAME}"
STATE_DIR="${STATE_DIR:-/var/lib/v2sudoku}"
RELEASE_VERSION="${RELEASE_VERSION:-latest}"
GO_VERSION="${GO_VERSION:-1.26.4}"
MIN_GO_VERSION="${MIN_GO_VERSION:-1.26.4}"
GO_BIN=""
BUILD_FROM_SOURCE=0
UPGRADE_ONLY=0
UNINSTALL_ONLY=0
PURGE=0
GITHUB_AUTH_TOKEN="${V2SUDOKU_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

API_HOST=""
API_KEY=""
NODE_IDS=()

usage() {
  cat <<'EOF'
Usage:
  bash install.sh --api-host https://panel.example.com --node-id 1 --api-key your-token
  bash install.sh --api-host https://panel.example.com --node-id 1,2,3 --api-key your-token
  bash install.sh --upgrade
  bash install.sh --uninstall

Optional flags:
  --upgrade
  --uninstall
  --purge
  --node-id ID[,ID...]
  --node-ids ID[,ID...]
  --version TAG
  --build-from-source
  --repo-url URL
  --repo-branch BRANCH
  --install-dir PATH
  --config-path PATH
  --service-name NAME
  --go-version VERSION

Private repository:
  Set V2SUDOKU_GITHUB_TOKEN to a GitHub token with repo read access.
EOF
}

log() {
  echo "[v2sudoku] $*"
}

fail() {
  echo "[v2sudoku] ERROR: $*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"
}

version_ge() {
  local current="$1"
  local minimum="$2"
  [[ "$(printf '%s\n%s\n' "$minimum" "$current" | sort -V | head -n1)" == "$minimum" ]]
}

github_curl() {
  local url="$1"
  local output="$2"
  local args=(-fsSL --connect-timeout 15 --retry 3)
  if [[ -n "$GITHUB_AUTH_TOKEN" ]]; then
    args+=(-H "Authorization: Bearer ${GITHUB_AUTH_TOKEN}")
    args+=(-H "X-GitHub-Api-Version: 2022-11-28")
  fi
  curl "${args[@]}" "$url" -o "$output"
}

git_with_auth() {
  if [[ -n "$GITHUB_AUTH_TOKEN" && "$REPO_URL" == https://github.com/* ]]; then
    git -c "http.https://github.com/.extraheader=AUTHORIZATION: bearer ${GITHUB_AUTH_TOKEN}" "$@"
    return
  fi
  git "$@"
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

node_id_exists() {
  local candidate="$1"
  local existing
  for existing in "${NODE_IDS[@]:-}"; do
    [[ -n "$existing" ]] || continue
    if [[ "$existing" == "$candidate" ]]; then
      return 0
    fi
  done
  return 1
}

add_node_ids() {
  local raw="$1"
  local item
  local -a parts=()
  IFS=',' read -r -a parts <<<"$raw"
  for item in "${parts[@]}"; do
    item="$(trim "$item")"
    [[ -n "$item" ]] || continue
    [[ "$item" =~ ^[0-9]+$ ]] || fail "invalid node id: ${item}"
    [[ "$item" -gt 0 ]] || fail "invalid node id: ${item}"
    if ! node_id_exists "$item"; then
      NODE_IDS+=("$item")
    fi
  done
}

node_ids_csv() {
  local joined=""
  local node_id
  for node_id in "${NODE_IDS[@]:-}"; do
    [[ -n "$node_id" ]] || continue
    if [[ -n "$joined" ]]; then
      joined+=","
    fi
    joined+="$node_id"
  done
  printf '%s' "$joined"
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --api-host)
        API_HOST="${2:-}"
        shift 2
        ;;
      --node-id|--node-ids)
        add_node_ids "${2:-}"
        shift 2
        ;;
      --api-key)
        API_KEY="${2:-}"
        shift 2
        ;;
      --repo-url)
        REPO_URL="${2:-}"
        shift 2
        ;;
      --version)
        RELEASE_VERSION="${2:-}"
        shift 2
        ;;
      --build-from-source)
        BUILD_FROM_SOURCE=1
        shift 1
        ;;
      --upgrade)
        UPGRADE_ONLY=1
        shift 1
        ;;
      --uninstall)
        UNINSTALL_ONLY=1
        shift 1
        ;;
      --purge)
        PURGE=1
        shift 1
        ;;
      --repo-branch)
        REPO_BRANCH="${2:-}"
        shift 2
        ;;
      --install-dir)
        INSTALL_DIR="${2:-}"
        SRC_DIR="${INSTALL_DIR}/src"
        BIN_PATH="${INSTALL_DIR}/v2sudoku"
        shift 2
        ;;
      --config-path)
        CONFIG_PATH="${2:-}"
        CONFIG_DIR="$(dirname "$CONFIG_PATH")"
        shift 2
        ;;
      --service-name)
        SERVICE_NAME="${2:-}"
        SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
        LOGROTATE_PATH="/etc/logrotate.d/${SERVICE_NAME}"
        shift 2
        ;;
      --go-version)
        GO_VERSION="${2:-}"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        fail "unknown argument: $1"
        ;;
    esac
  done

  if [[ "$UNINSTALL_ONLY" -eq 1 ]]; then
    return
  fi

  if [[ "$UPGRADE_ONLY" -eq 1 ]]; then
    [[ -f "$CONFIG_PATH" ]] || fail "--upgrade requires existing config: ${CONFIG_PATH}"
  else
    [[ -n "$API_HOST" ]] || fail "--api-host is required"
    [[ "${#NODE_IDS[@]}" -gt 0 ]] || fail "--node-id is required"
    [[ -n "$API_KEY" ]] || fail "--api-key is required"
  fi
}

ensure_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    fail "please run as root"
  fi
}

install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y curl git tar xz-utils ca-certificates
    return
  fi
  if command -v dnf >/dev/null 2>&1; then
    dnf install -y curl git tar xz ca-certificates
    return
  fi
  if command -v yum >/dev/null 2>&1; then
    yum install -y curl git tar xz ca-certificates
    return
  fi
  fail "unsupported package manager"
}

normalize_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      fail "unsupported architecture: $(uname -m)"
      ;;
  esac
}

release_url() {
  local asset_name="$1"
  if [[ "$RELEASE_VERSION" == "latest" ]]; then
    echo "https://github.com/${REPO_SLUG}/releases/latest/download/${asset_name}"
    return
  fi
  echo "https://github.com/${REPO_SLUG}/releases/download/${RELEASE_VERSION}/${asset_name}"
}

download_release_binary() {
  local arch
  arch="$(normalize_arch)"
  local asset_name="v2sudoku_linux_${arch}.tar.gz"
  local url
  url="$(release_url "$asset_name")"
  local workdir
  workdir="$(mktemp -d /tmp/v2sudoku-release.XXXXXX)"
  local archive="${workdir}/${asset_name}"

  log "downloading release package ${asset_name}"
  if ! github_curl "$url" "$archive" 2>/dev/null; then
    rm -rf "$workdir"
    return 1
  fi

  tar -xzf "$archive" -C "$workdir"
  [[ -f "${workdir}/v2sudoku" ]] || fail "release archive missing v2sudoku binary"
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "${workdir}/v2sudoku" "$BIN_PATH"
  rm -rf "$workdir"
}

ensure_go() {
  local current=""
  if command -v go >/dev/null 2>&1; then
    current="$(go version | awk '{print $3}' | sed 's/^go//')"
  fi

  if [[ -n "$current" ]] && version_ge "$current" "$MIN_GO_VERSION"; then
    log "using existing Go ${current}"
    GO_BIN="$(command -v go)"
    return
  fi

  local arch
  arch="$(normalize_arch)"
  local tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
  local url="https://go.dev/dl/${tarball}"
  local tmpfile
  tmpfile="$(mktemp /tmp/v2sudoku-go.XXXXXX.tar.gz)"

  log "installing Go ${GO_VERSION}"
  curl -fsSL "$url" -o "$tmpfile"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmpfile"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  rm -f "$tmpfile"
  need_cmd go
  GO_BIN="$(command -v go)"
}

sync_repo() {
  mkdir -p "$INSTALL_DIR"
  if [[ -d "${SRC_DIR}/.git" ]]; then
    log "updating repository"
    git_with_auth -C "$SRC_DIR" fetch --tags origin
    git_with_auth -C "$SRC_DIR" checkout "$REPO_BRANCH"
    git_with_auth -C "$SRC_DIR" reset --hard "origin/${REPO_BRANCH}"
  else
    rm -rf "$SRC_DIR"
    log "cloning repository"
    git_with_auth clone --depth 1 --branch "$REPO_BRANCH" "$REPO_URL" "$SRC_DIR"
  fi
}

build_binary() {
  log "building v2sudoku"
  mkdir -p "$INSTALL_DIR"
  (
    cd "$SRC_DIR"
    local version
    local commit
    version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
    commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
    "$GO_BIN" build -trimpath -ldflags="-s -w -X github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/version.Version=${version} -X github.com/SUDOKU-ASCII/sudoku/v2sudoku/internal/version.Commit=${commit}" -o "$BIN_PATH" .
  )
  chmod 0755 "$BIN_PATH"
}

install_binary() {
  if [[ "$BUILD_FROM_SOURCE" -eq 0 ]] && download_release_binary; then
    log "installed from GitHub release"
    return
  fi

  if [[ "$BUILD_FROM_SOURCE" -eq 0 ]]; then
    log "release package not available, falling back to source build"
  fi
  ensure_go
  sync_repo
  build_binary
}

write_config() {
  mkdir -p "$CONFIG_DIR" "$LOG_DIR" "$STATE_DIR"
  touch "${LOG_DIR}/v2sudoku.log"

  if [[ -f "$CONFIG_PATH" ]]; then
    cp "$CONFIG_PATH" "${CONFIG_PATH}.bak.$(date +%s)"
  fi

  cat >"$CONFIG_PATH" <<EOF
Log:
  Level: info
  Output: ${LOG_DIR}/v2sudoku.log

Runtime:
  Engine: embedded
  WorkingDir: ${STATE_DIR}
  FallbackAddress: 127.0.0.1:80
  ClientKeySource: deterministic_split
  ClientKeyFile: ${STATE_DIR}/client-keys.json

Nodes:
EOF

  local node_id
  for node_id in "${NODE_IDS[@]}"; do
    cat >>"$CONFIG_PATH" <<EOF
  - ApiHost: "${API_HOST}"
    NodeID: ${node_id}
    ApiKey: "${API_KEY}"
    Timeout: 15
    RetryCount: 2
EOF
  done
  chmod 0600 "$CONFIG_PATH"
}

write_service() {
  cat >"$SERVICE_PATH" <<EOF
[Unit]
Description=v2sudoku service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BIN_PATH} -config ${CONFIG_PATH}
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
}

write_logrotate() {
  cat >"$LOGROTATE_PATH" <<EOF
${LOG_DIR}/*.log {
  daily
  rotate 1
  size 50M
  missingok
  notifempty
  copytruncate
}
EOF
}

start_service() {
  need_cmd systemctl
  systemctl daemon-reload
  systemctl enable --now "$SERVICE_NAME"
  systemctl restart "$SERVICE_NAME"
  systemctl --no-pager --full status "$SERVICE_NAME" || true
}

uninstall_service() {
  need_cmd systemctl
  if systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1 || [[ -f "$SERVICE_PATH" ]]; then
    systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
  fi
  rm -f "$SERVICE_PATH"
  systemctl daemon-reload
  systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
}

uninstall_v2sudoku() {
  uninstall_service
  rm -f "$LOGROTATE_PATH"
  rm -rf "$INSTALL_DIR"

  if [[ "$PURGE" -eq 1 ]]; then
    rm -rf "$CONFIG_DIR" "$LOG_DIR" "$STATE_DIR"
    log "uninstalled and purged v2sudoku"
  else
    log "uninstalled v2sudoku; kept config, logs and state"
    log "kept config: ${CONFIG_DIR}"
    log "kept logs: ${LOG_DIR}"
    log "kept state: ${STATE_DIR}"
  fi
}

main() {
  parse_args "$@"
  ensure_root
  if [[ "$UNINSTALL_ONLY" -eq 1 ]]; then
    uninstall_v2sudoku
    return
  fi
  install_packages
  install_binary
  if [[ "$UPGRADE_ONLY" -eq 0 ]]; then
    write_config
  fi
  write_service
  write_logrotate
  start_service
  if [[ "$UPGRADE_ONLY" -eq 1 ]]; then
    log "upgraded successfully"
  else
    log "installed successfully"
    log "node ids: $(node_ids_csv)"
  fi
  log "config: ${CONFIG_PATH}"
  log "service: ${SERVICE_NAME}"
}

main "$@"

