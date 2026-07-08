#!/usr/bin/env bash
set -euo pipefail

REPO="cloudapp3/vmflow"
BINARY_NAME="vmflow"
INSTALL_DIR="${VMFLOW_INSTALL_DIR:-}"
VERSION=""
SKIP_VERIFY=0
UNINSTALL=0

usage() {
  cat <<'EOF'
Install vmflow from GitHub Releases.

Usage:
  install.sh [--version vX.Y.Z] [--dir PATH] [--skip-verify] [--uninstall]

Options:
  --version <tag>   Install a specific release tag. Defaults to the latest release.
  --dir <path>      Install directory. Auto-detected in order: /usr/local/bin,
                    ~/.local/bin, ~/bin. Override with VMFLOW_INSTALL_DIR env var.
  --skip-verify     Skip SHA-256 checksum verification.
  --uninstall       Uninstall vmflow instead of installing. Stops the service
                    and removes the binary, config, logs, and cache.
  -h, --help        Show this help message.

Examples:
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | sudo bash -s -- --dir /usr/local/bin
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash -s -- --version v0.1.0
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | sudo bash -s -- --uninstall
EOF
}

log() {
  printf '%s\n' "$*" >&2
}

die() {
  log "Error: $*"
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

path_has_dir() (
  dir="$1"

  while [ "$dir" != "/" ] && [ "${dir%/}" != "$dir" ]; do
    dir="${dir%/}"
  done
  [ -n "$dir" ] || return 1

  IFS=:
  for path_dir in ${PATH:-}; do
    while [ "$path_dir" != "/" ] && [ "${path_dir%/}" != "$path_dir" ]; do
      path_dir="${path_dir%/}"
    done
    [ "$path_dir" = "$dir" ] && return 0
  done

  return 1
)

print_path_hint() {
  local install_dir="$1"
  local target_path="$2"

  if path_has_dir "$install_dir"; then
    log "Verify the installation with:"
    log "  ${BINARY_NAME} version"
    return
  fi

  log "Warning: ${install_dir} is not in your PATH, so running \"${BINARY_NAME}\" may fail."
  log ""
  log "You can run it directly with:"
  log "  \"${target_path}\" version"
  log ""
  if [ "$install_dir" != "/usr/local/bin" ]; then
    log "To make \"${BINARY_NAME}\" available globally, either create a symlink:"
    log "  sudo ln -sf \"${target_path}\" \"/usr/local/bin/${BINARY_NAME}\""
    log ""
  fi
  log "Or add ${install_dir} to your shell PATH:"
  log "  export PATH=\"${install_dir}:\$PATH\""
}

# stop_service best-effort stops and removes the native service (systemd on
# Linux, launchd on macOS). Windows is not supported by install.sh.
stop_service() {
  if command -v systemctl >/dev/null 2>&1; then
    systemctl stop "$BINARY_NAME" 2>/dev/null || true
    systemctl disable "$BINARY_NAME" 2>/dev/null || true
    rm -f "/etc/systemd/system/${BINARY_NAME}.service" 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
  elif command -v launchctl >/dev/null 2>&1; then
    launchctl bootout "system/io.cloudapp.${BINARY_NAME}" 2>/dev/null || true
    rm -f "/Library/LaunchDaemons/io.cloudapp.${BINARY_NAME}.plist" 2>/dev/null || true
  fi
}

# do_uninstall removes vmflow. It delegates to `vmflow uninstall` when the
# installed binary supports it (full cleanup incl. certificates); otherwise it
# falls back to a shell-level removal of service/binary/config/logs/cache.
do_uninstall() {
  # Locate the installed binary: explicit --dir, then PATH, then common spots.
  TARGET=""
  if [ -n "$INSTALL_DIR" ]; then
    TARGET="${INSTALL_DIR}/${BINARY_NAME}"
  elif command -v "$BINARY_NAME" >/dev/null 2>&1; then
    TARGET="$(command -v "$BINARY_NAME")"
  else
    for d in /usr/local/bin /usr/bin "$HOME/.local/bin" "$HOME/bin"; do
      if [ -x "$d/$BINARY_NAME" ]; then TARGET="$d/$BINARY_NAME"; break; fi
    done
  fi

  # Delegate to the binary's own uninstall when available (richest cleanup).
  if [ -n "$TARGET" ] && [ -x "$TARGET" ] && "$TARGET" uninstall --help >/dev/null 2>&1; then
    log "Delegating to: $TARGET uninstall"
    # Read confirmation from the user's tty: under `curl | bash`, stdin is the
    # download pipe and cannot serve the [y/N] prompt.
    exec "$TARGET" uninstall </dev/tty
  fi

  log "vmflow binary not found (or too old for self-uninstall) at ${TARGET:-<none>}; cleaning up via shell."

  case "$(uname -s)" in
    Linux)  CFG="/etc/vmflow/config.yaml" ;;
    Darwin) CFG="/usr/local/etc/vmflow/config.yaml" ;;
    *)      CFG="" ;;
  esac
  CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/vmflow"

  # Build the removal plan (only entries that exist).
  PLAN=""
  if [ -n "$TARGET" ] && [ -x "$TARGET" ]; then PLAN="$PLAN $TARGET"; fi
  if [ -n "$CFG" ] && [ -e "$CFG" ]; then PLAN="$PLAN $CFG"; fi
  if [ -d /var/log/vmflow ]; then PLAN="$PLAN /var/log/vmflow"; fi
  if [ -d "$CACHE_DIR" ]; then PLAN="$PLAN $CACHE_DIR"; fi

  if [ -z "$PLAN" ]; then
    stop_service
    log "Nothing left to remove."
    exit 0
  fi

  log "The following will be removed:"
  for t in $PLAN; do log "  - $t"; done

  # Confirm only when stdin is a terminal; skip under `curl | bash` pipes.
  if [ -t 0 ]; then
    printf 'Proceed with uninstall? [y/N] ' >&2
    read -r ans
    case "$ans" in
      y|Y|yes|YES) ;;
      *) log "aborted"; exit 0 ;;
    esac
  fi

  stop_service

  for t in $PLAN; do
    if rm -rf -- "$t" 2>/dev/null; then
      log "removed $t"
    else
      log "warning: could not remove $t (try running with sudo)"
    fi
  done

  log "vmflow uninstalled."
  log "Note: TLS/ACME certificate files referenced by the config are not purged by this shell path; install the binary and run \`vmflow uninstall\` to remove them too."
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      shift
      [ "$#" -gt 0 ] || die "missing value for --version"
      VERSION="$1"
      ;;
    --version=*)
      VERSION="${1#*=}"
      ;;
    --dir)
      shift
      [ "$#" -gt 0 ] || die "missing value for --dir"
      INSTALL_DIR="$1"
      ;;
    --dir=*)
      INSTALL_DIR="${1#*=}"
      ;;
    --skip-verify)
      SKIP_VERIFY=1
      ;;
    --uninstall)
      UNINSTALL=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
  shift
done

if [ "$UNINSTALL" -eq 1 ]; then
  do_uninstall
  exit 0
fi

require_cmd curl
require_cmd tar
require_cmd uname
require_cmd mktemp

# Auto-detect install directory if not explicitly set
if [ -z "$INSTALL_DIR" ]; then
  for d in /usr/local/bin "$HOME/.local/bin" "$HOME/bin"; do
    if [ -w "$d" ] 2>/dev/null || [ -w "$(dirname "$d")" ] 2>/dev/null; then
      INSTALL_DIR="$d"
      break
    fi
  done
  INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
fi

case "$(uname -s)" in
  Linux) OS="linux" ;;
  Darwin) OS="darwin" ;;
  *)
    die "unsupported operating system: $(uname -s)"
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    die "unsupported architecture: $(uname -m)"
    ;;
esac

TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
CURL_ARGS=(-fsSL)
if [ -n "${TOKEN}" ]; then
  CURL_ARGS+=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "Accept: application/vnd.github+json"
    -H "X-GitHub-Api-Version: 2022-11-28"
  )
fi

curl_text() {
  curl "${CURL_ARGS[@]}" "$1"
}

curl_download() {
  local url="$1"
  local output="$2"
  curl "${CURL_ARGS[@]}" -o "$output" "$url"
}

if [ -z "$VERSION" ]; then
  log "Resolving latest release from ${REPO}..."
  if ! VERSION="$(
    curl_text "https://api.github.com/repos/${REPO}/releases/latest" \
      | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' \
      | head -n 1
  )"; then
    die "failed to query latest release from ${REPO}"
  fi
  [ -n "$VERSION" ] || die "failed to resolve latest release tag"
fi

case "$VERSION" in
  v*) ;;
  *) VERSION="v${VERSION}" ;;
esac

VERSION_NO_V="${VERSION#v}"
ARCHIVE_NAME="${BINARY_NAME}-${VERSION_NO_V}-${OS}-${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
ARCHIVE_URL="${BASE_URL}/${ARCHIVE_NAME}"
CHECKSUMS_URL="${BASE_URL}/checksums.txt"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

ARCHIVE_PATH="${TMPDIR}/${ARCHIVE_NAME}"
CHECKSUMS_PATH="${TMPDIR}/checksums.txt"

log "Downloading ${ARCHIVE_NAME}..."
curl_download "$ARCHIVE_URL" "$ARCHIVE_PATH" || die "failed to download ${ARCHIVE_URL}"

if [ "$SKIP_VERIFY" -eq 0 ]; then
  log "Downloading checksums.txt..."
  curl_download "$CHECKSUMS_URL" "$CHECKSUMS_PATH" || die "failed to download ${CHECKSUMS_URL}"

  EXPECTED_SUM="$(
    awk -v name="$ARCHIVE_NAME" '$2 == name { print $1 }' "$CHECKSUMS_PATH"
  )"
  [ -n "$EXPECTED_SUM" ] || die "checksum entry not found for ${ARCHIVE_NAME}"

  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_SUM="$(sha256sum "$ARCHIVE_PATH" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL_SUM="$(shasum -a 256 "$ARCHIVE_PATH" | awk '{print $1}')"
  else
    die "checksum verification requested but neither sha256sum nor shasum is available"
  fi

  [ "$ACTUAL_SUM" = "$EXPECTED_SUM" ] || die "checksum mismatch for ${ARCHIVE_NAME}"
  log "Checksum verified."
else
  log "Skipping checksum verification."
fi

log "Extracting archive..."
# --no-same-owner: do not restore attacker-controlled uid/gid from the archive
# when this script runs as root (e.g. curl|sudo bash). Extraction still happens
# in a private mktemp dir and we only install the single binary by name.
tar --no-same-owner -xzf "$ARCHIVE_PATH" -C "$TMPDIR"

BINARY_PATH="${TMPDIR}/${BINARY_NAME}"
if [ ! -f "$BINARY_PATH" ]; then
  BINARY_PATH="$(find "$TMPDIR" -maxdepth 2 -type f -name "$BINARY_NAME" | head -n 1 || true)"
fi
[ -n "${BINARY_PATH}" ] && [ -f "$BINARY_PATH" ] || die "failed to find ${BINARY_NAME} in archive"

if [ ! -d "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR" 2>/dev/null || die "cannot create install directory: ${INSTALL_DIR}"
fi
[ -w "$INSTALL_DIR" ] || die "install directory is not writable: ${INSTALL_DIR}"

TARGET_PATH="${INSTALL_DIR}/${BINARY_NAME}"
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$BINARY_PATH" "$TARGET_PATH" \
    || die "failed to install ${BINARY_NAME} to ${TARGET_PATH}"
else
  cp "$BINARY_PATH" "$TARGET_PATH" \
    || die "failed to copy ${BINARY_NAME} to ${TARGET_PATH}"
  chmod 0755 "$TARGET_PATH" \
    || die "failed to chmod ${TARGET_PATH}"
fi

log "Installed ${BINARY_NAME} ${VERSION} to ${TARGET_PATH}"

print_path_hint "$INSTALL_DIR" "$TARGET_PATH"
