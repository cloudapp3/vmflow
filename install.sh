#!/usr/bin/env bash
set -euo pipefail

REPO="cloudapp3/vmflow"
BINARY_NAME="vmflow"
CONFIG_NAME="config.yaml"
CONFIG_MARKER_NAME=".vmflow-config-owned"
INSTALL_DIR="${VMFLOW_INSTALL_DIR:-}"
INSTALL_DIR_EXPLICIT=0
VERSION=""
SKIP_VERIFY=0
UNINSTALL=0
SYSTEM_INSTALL=0
NO_MODIFY_PATH="${VMFLOW_NO_MODIFY_PATH:-0}"
PRINT_INSTALL_DIR=0
USE_SUDO=0
SUDO_BIN="${VMFLOW_SUDO:-}"
PATH_RC=""
REUSING_INSTALL=0

if [ -n "$INSTALL_DIR" ]; then
  INSTALL_DIR_EXPLICIT=1
fi

usage() {
  cat <<'EOF'
Install vmflow from GitHub Releases.

Usage:
  install.sh [--version vX.Y.Z] [--dir PATH] [--system] [--skip-verify]
             [--no-modify-path] [--print-install-dir] [--uninstall]

Options:
  --version <tag>   Install a specific release tag. Defaults to the latest release.
  --dir <path>      Install directory. Overrides automatic root/user selection.
                    Also available through VMFLOW_INSTALL_DIR.
  --system          Install system-wide to /usr/local/bin (or the explicit --dir).
                    Uses sudo only for target checks and writes when needed.
                    A newly created config remains root-owned with mode 0600.
  --skip-verify     Skip SHA-256 checksum verification.
  --no-modify-path  Do not add an automatic user install to a shell startup file.
  --print-install-dir
                    Print the selected install directory to stdout after success.
  --uninstall       Uninstall vmflow instead of installing. Stops the service
                    and removes the binary, config, logs, and cache.
  -h, --help        Show this help message.

Examples:
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash -s -- --system
  curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash
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

# create_exclusive_file opens target once with noclobber enabled. This prevents
# a path inserted after validation from redirecting a privileged write through
# a symlink or overwriting an operator-created file.
create_exclusive_file() (
  target="$1"
  shift
  umask 077
  set -o noclobber
  "$@" >"$target"
)

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

run_privileged() {
  local command_name
  local command_path

  if [ "$USE_SUDO" -eq 1 ]; then
    command_name="$1"
    shift
    case "$command_name" in
      test)
        for command_path in /usr/bin/test /bin/test; do
          [ -x "$command_path" ] && break
        done
        ;;
      mkdir)
        for command_path in /usr/bin/mkdir /bin/mkdir; do
          [ -x "$command_path" ] && break
        done
        ;;
      *)
        die "internal error: no trusted privileged command path for ${command_name}"
        ;;
    esac
    [ -x "$command_path" ] \
      || die "required privileged command not found: ${command_name}"
    "$SUDO_BIN" "$command_path" "$@"
  else
    "$@"
  fi
}

create_exclusive_from_file() {
  local target="$1"
  local source="$2"

  if [ "$USE_SUDO" -eq 1 ]; then
    "$SUDO_BIN" /bin/sh -c 'umask 077; set -C; /bin/cat >"$1"' vmflow-install "$target" <"$source"
  else
    create_exclusive_file "$target" cat "$source"
  fi
}

create_exclusive_with_text() {
  local target="$1"
  local value="$2"

  if [ "$USE_SUDO" -eq 1 ]; then
    "$SUDO_BIN" /bin/sh -c 'umask 077; set -C; printf "%s" "$2" >"$1"' vmflow-install "$target" "$value"
  else
    create_exclusive_file "$target" printf '%s' "$value"
  fi
}

dir_can_be_created() {
  local dir="$1"
  local parent

  while [ ! -e "$dir" ]; do
    parent="$(dirname "$dir")"
    [ "$parent" != "$dir" ] || return 1
    dir="$parent"
  done

  [ -d "$dir" ] && [ -w "$dir" ]
}

prepare_system_privileges() {
  [ "$SYSTEM_INSTALL" -eq 1 ] || return 0

  if [ "$(id -u)" -eq 0 ]; then
    return 0
  fi

  if [ -n "$SUDO_BIN" ]; then
    case "$SUDO_BIN" in
      /*) ;;
      *) die "VMFLOW_SUDO must be an absolute path" ;;
    esac
    [ -x "$SUDO_BIN" ] || die "configured sudo command is not executable: ${SUDO_BIN}"
  else
    for candidate in /usr/bin/sudo /bin/sudo; do
      if [ -x "$candidate" ]; then
        SUDO_BIN="$candidate"
        break
      fi
    done
    [ -n "$SUDO_BIN" ] || die "system installation requires root or sudo"
  fi

  if "$SUDO_BIN" -n -v >/dev/null 2>&1; then
    USE_SUDO=1
    return 0
  fi

  [ -e /dev/tty ] || die "system installation requires root or an interactive sudo session"
  "$SUDO_BIN" -v </dev/tty || die "sudo authentication failed; rerun as root or without --system"
  USE_SUDO=1
}

is_reusable_install_dir() {
  local dir="$1"

  [ -x "$dir/$BINARY_NAME" ] && [ -f "$dir/$BINARY_NAME" ] \
    && [ ! -L "$dir/$BINARY_NAME" ] \
    && [ -f "$dir/$CONFIG_NAME" ] && [ ! -L "$dir/$CONFIG_NAME" ] \
    && dir_can_be_created "$dir"
}

select_install_dir() {
  local existing_binary
  local existing_dir
  local d

  [ -n "$INSTALL_DIR" ] && return 0

  if [ "$SYSTEM_INSTALL" -eq 1 ]; then
    INSTALL_DIR="/usr/local/bin"
    return 0
  fi

  existing_binary="$(command -v "$BINARY_NAME" 2>/dev/null || true)"
  if [ -n "$existing_binary" ] && [ -x "$existing_binary" ] \
    && [ -f "$existing_binary" ] && [ ! -L "$existing_binary" ]; then
    existing_dir="$(dirname "$existing_binary")"
    case "$existing_dir" in
      "$HOME/.local/bin"|"$HOME/bin"|/usr/local/bin)
        if is_reusable_install_dir "$existing_dir"; then
          INSTALL_DIR="$existing_dir"
          REUSING_INSTALL=1
          return 0
        fi
        ;;
    esac
  fi

  # Preserve the colocated config chosen by earlier installer versions when
  # vmflow is not currently in PATH. This avoids creating a second binary/config
  # pair for root users upgrading from the former ~/.local/bin quick start.
  for d in "$HOME/.local/bin" "$HOME/bin" /usr/local/bin; do
    if is_reusable_install_dir "$d"; then
      INSTALL_DIR="$d"
      REUSING_INSTALL=1
      return 0
    fi
  done

  if [ "$(id -u)" -eq 0 ]; then
    INSTALL_DIR="/usr/local/bin"
    return 0
  fi

  # Prefer a conventional user bin directory only when it is already in PATH.
  # Otherwise the fallback below will add ~/.local/bin to the user's shell
  # startup file and the caller can export it for the current shell.
  for d in "$HOME/.local/bin" "$HOME/bin"; do
    if path_has_dir "$d" && dir_can_be_created "$d"; then
      INSTALL_DIR="$d"
      return 0
    fi
  done

  for d in "$HOME/.local/bin" "$HOME/bin"; do
    if dir_can_be_created "$d"; then
      INSTALL_DIR="$d"
      return 0
    fi
  done

  INSTALL_DIR="$HOME/.local/bin"
}

configure_user_path() {
  local install_dir="$1"
  local shell_path="${SHELL:-}"
  local shell_name="${shell_path##*/}"
  local rc_file
  local path_line
  local quoted_install_dir

  [ "$SYSTEM_INSTALL" -eq 0 ] || return 0
  [ "$INSTALL_DIR_EXPLICIT" -eq 0 ] || return 0
  [ "$NO_MODIFY_PATH" != "1" ] || return 0
  path_has_dir "$install_dir" && return 0
  case "$install_dir" in
    "$HOME"/*) ;;
    *) return 0 ;;
  esac

  case "$shell_name" in
    zsh)  rc_file="$HOME/.zshrc" ;;
    bash) rc_file="$HOME/.bashrc" ;;
    sh|dash|ksh) rc_file="$HOME/.profile" ;;
    *)
      log "Warning: unsupported shell ${shell_name:-<unknown>}; PATH startup file was not modified."
      return 0
      ;;
  esac

  if [ -L "$rc_file" ] || { [ -e "$rc_file" ] && [ ! -f "$rc_file" ]; }; then
    log "Warning: cannot update shell PATH because ${rc_file} is not a regular file."
    return 0
  fi
  if [ -e "$rc_file" ] && [ ! -w "$rc_file" ]; then
    log "Warning: cannot update shell PATH because ${rc_file} is not writable."
    return 0
  fi

  printf -v quoted_install_dir '%q' "$install_dir"
  path_line="export PATH=${quoted_install_dir}:\$PATH"
  if [ ! -f "$rc_file" ] || ! grep -Fqx "$path_line" "$rc_file" 2>/dev/null; then
    if ! printf '\n# vmflow user install\n%s\n' "$path_line" >>"$rc_file"; then
      log "Warning: could not add ${install_dir} to ${rc_file}."
      return 0
    fi
  fi

  PATH_RC="$rc_file"
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
  local config_path="$3"

  if [ "$USE_SUDO" -eq 1 ]; then
    log "Verify the root-owned system installation with:"
    log "  \"${SUDO_BIN}\" \"${target_path}\" version"
  elif path_has_dir "$install_dir"; then
    log "Verify the installation with:"
    log "  ${BINARY_NAME} version"
  else
    if [ -n "$PATH_RC" ]; then
      log "Added ${install_dir} to ${PATH_RC}."
      log "Reload it with:"
      log "  . \"${PATH_RC}\""
    else
      log "Warning: ${install_dir} is not in your PATH, so running \"${BINARY_NAME}\" may fail."
    fi
    log ""
    log "You can run it directly with:"
    log "  \"${target_path}\" version"
    log ""
    log "To use \"${BINARY_NAME}\" in this shell, run:"
    log "  export PATH=\"${install_dir}:\$PATH\""
  fi

  log ""
  log "Review forwarding rules before starting or restarting vmflow:"
  log "  ${config_path}"
  log "Upgrades preserve existing rules, including enabled public listeners."
  log ""
  if [ "$USE_SUDO" -eq 1 ]; then
    log "Start vmflow as root (loads ${config_path} by default):"
    log "  \"${SUDO_BIN}\" \"${target_path}\""
  else
    log "Start vmflow (loads ${config_path} by default):"
    log "  \"${target_path}\""
  fi
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
# installed binary supports it; otherwise it falls back to a shell-level
# removal of service/binary/config/logs/cache.
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
    Linux)  SYSTEM_CFG="/etc/vmflow/config.yaml" ;;
    Darwin) SYSTEM_CFG="/usr/local/etc/vmflow/config.yaml" ;;
    *)      SYSTEM_CFG="" ;;
  esac
  COLOCATED_CFG=""
  COLOCATED_CFG_MARKER=""
  if [ -n "$TARGET" ]; then
    COLOCATED_CFG="$(dirname "$TARGET")/${CONFIG_NAME}"
    COLOCATED_CFG_MARKER="$(dirname "$TARGET")/${CONFIG_MARKER_NAME}"
  fi
  CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/vmflow"
  CACHE_FILE="${CACHE_DIR}/update-check.json"

  # Build the removal plan (only entries that exist).
  PLAN=()
  if [ -n "$TARGET" ] && [ -x "$TARGET" ]; then PLAN+=("$TARGET"); fi
  if [ -n "$COLOCATED_CFG_MARKER" ] && [ -f "$COLOCATED_CFG_MARKER" ] && [ ! -L "$COLOCATED_CFG_MARKER" ]; then
    if [ -e "$COLOCATED_CFG" ] || [ -L "$COLOCATED_CFG" ]; then PLAN+=("$COLOCATED_CFG"); fi
    PLAN+=("$COLOCATED_CFG_MARKER")
  elif [ -n "$COLOCATED_CFG" ] && { [ -e "$COLOCATED_CFG" ] || [ -L "$COLOCATED_CFG" ]; }; then
    log "Preserving unowned colocated config: $COLOCATED_CFG"
  fi
  if [ -n "$SYSTEM_CFG" ] && [ "$SYSTEM_CFG" != "$COLOCATED_CFG" ] && [ -e "$SYSTEM_CFG" ]; then PLAN+=("$SYSTEM_CFG"); fi
  if [ -d /var/log/vmflow ]; then PLAN+=("/var/log/vmflow"); fi
  if [ -f "$CACHE_FILE" ]; then PLAN+=("$CACHE_FILE"); fi

  if [ "${#PLAN[@]}" -eq 0 ]; then
    stop_service
    log "Nothing left to remove."
    exit 0
  fi

  log "The following will be removed:"
  for t in "${PLAN[@]}"; do log "  - $t"; done

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

  for t in "${PLAN[@]}"; do
    if [ -n "$COLOCATED_CFG" ] && [ "$t" = "$COLOCATED_CFG" ]; then
      if [ ! -f "$COLOCATED_CFG_MARKER" ] || [ -L "$COLOCATED_CFG_MARKER" ]; then
        log "warning: preserving colocated config because its ownership marker changed: $t"
        continue
      fi
    fi

    if [ "$t" = "/var/log/vmflow" ]; then
      remove_cmd=(rm -rf -- "$t")
    elif [ -d "$t" ] && [ ! -L "$t" ]; then
      log "warning: refusing to recursively remove a directory planned as a file: $t"
      continue
    else
      remove_cmd=(rm -f -- "$t")
    fi

    if "${remove_cmd[@]}" 2>/dev/null; then
      log "removed $t"
    else
      log "warning: could not remove $t (try running with sudo)"
    fi
  done

  # Remove the cache directory only when it is empty. It may live under a
  # caller-provided XDG_CACHE_HOME and must never be recursively purged.
  rmdir "$CACHE_DIR" 2>/dev/null || true

  log "vmflow uninstalled."
  log "External TLS certificate and key files referenced by the config are preserved."
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
      [ -n "$INSTALL_DIR" ] || die "empty value for --dir"
      INSTALL_DIR_EXPLICIT=1
      ;;
    --dir=*)
      INSTALL_DIR="${1#*=}"
      [ -n "$INSTALL_DIR" ] || die "empty value for --dir"
      INSTALL_DIR_EXPLICIT=1
      ;;
    --system)
      SYSTEM_INSTALL=1
      ;;
    --skip-verify)
      SKIP_VERIFY=1
      ;;
    --no-modify-path)
      NO_MODIFY_PATH=1
      ;;
    --print-install-dir)
      PRINT_INSTALL_DIR=1
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
require_cmd id
require_cmd dirname
require_cmd grep
require_cmd mkdir
require_cmd cp
require_cmd chmod

select_install_dir
prepare_system_privileges
if [ "$REUSING_INSTALL" -eq 1 ]; then
  log "Using existing installation directory: ${INSTALL_DIR}"
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
# in a private mktemp dir and we only install the expected files by name.
tar --no-same-owner -xzf "$ARCHIVE_PATH" -C "$TMPDIR"

BINARY_PATH="${TMPDIR}/${BINARY_NAME}"
if [ ! -f "$BINARY_PATH" ]; then
  BINARY_PATH="$(find "$TMPDIR" -maxdepth 2 -type f -name "$BINARY_NAME" | head -n 1 || true)"
fi
[ -n "${BINARY_PATH}" ] && [ -f "$BINARY_PATH" ] || die "failed to find ${BINARY_NAME} in archive"

# Release archives place config.yaml beside the binary.
CONFIG_SOURCE="${TMPDIR}/${CONFIG_NAME}"
[ -f "$CONFIG_SOURCE" ] && [ ! -L "$CONFIG_SOURCE" ] \
  || die "release ${VERSION} is incompatible with this installer: expected a top-level regular ${CONFIG_NAME}"

if ! run_privileged test -d "$INSTALL_DIR"; then
  run_privileged mkdir -p "$INSTALL_DIR" 2>/dev/null \
    || die "cannot create install directory: ${INSTALL_DIR}"
fi
run_privileged test -w "$INSTALL_DIR" \
  || die "install directory is not writable: ${INSTALL_DIR}; use --system for a privileged install"

TARGET_PATH="${INSTALL_DIR}/${BINARY_NAME}"
TARGET_CONFIG_PATH="${INSTALL_DIR}/${CONFIG_NAME}"
TARGET_CONFIG_MARKER="${INSTALL_DIR}/${CONFIG_MARKER_NAME}"
if run_privileged test -L "$TARGET_PATH"; then
  die "existing binary path is a symbolic link: ${TARGET_PATH}"
fi
if run_privileged test -e "$TARGET_PATH" && ! run_privileged test -f "$TARGET_PATH"; then
  die "existing binary path is not a regular file: ${TARGET_PATH}"
fi
CONFIG_MARKER_EXISTS=0
if run_privileged test -e "$TARGET_CONFIG_MARKER" || run_privileged test -L "$TARGET_CONFIG_MARKER"; then
  if ! run_privileged test -f "$TARGET_CONFIG_MARKER" || run_privileged test -L "$TARGET_CONFIG_MARKER"; then
    die "existing config ownership marker is not a regular file: ${TARGET_CONFIG_MARKER}"
  fi
  CONFIG_MARKER_EXISTS=1
fi
if run_privileged test -e "$TARGET_CONFIG_PATH" || run_privileged test -L "$TARGET_CONFIG_PATH"; then
  if ! run_privileged test -f "$TARGET_CONFIG_PATH" || run_privileged test -L "$TARGET_CONFIG_PATH"; then
    die "existing config path is not a regular file: ${TARGET_CONFIG_PATH}"
  fi
  INSTALL_CONFIG=0
else
  INSTALL_CONFIG=1
fi

if [ "$USE_SUDO" -eq 1 ]; then
  PRIVILEGED_INSTALL=""
  for candidate in /usr/bin/install /bin/install; do
    if [ -x "$candidate" ]; then
      PRIVILEGED_INSTALL="$candidate"
      break
    fi
  done
  [ -n "$PRIVILEGED_INSTALL" ] \
    || die "system installation requires /usr/bin/install or /bin/install"
  "$SUDO_BIN" "$PRIVILEGED_INSTALL" -m 0755 "$BINARY_PATH" "$TARGET_PATH" \
    || die "failed to install ${BINARY_NAME} to ${TARGET_PATH}"
elif command -v install >/dev/null 2>&1; then
  run_privileged install -m 0755 "$BINARY_PATH" "$TARGET_PATH" \
    || die "failed to install ${BINARY_NAME} to ${TARGET_PATH}"
else
  run_privileged cp "$BINARY_PATH" "$TARGET_PATH" \
    || die "failed to copy ${BINARY_NAME} to ${TARGET_PATH}"
  run_privileged chmod 0755 "$TARGET_PATH" \
    || die "failed to chmod ${TARGET_PATH}"
fi

if [ "$INSTALL_CONFIG" -eq 1 ]; then
  create_exclusive_from_file "$TARGET_CONFIG_PATH" "$CONFIG_SOURCE" \
    || die "failed to create ${CONFIG_NAME} without overwriting an existing path: ${TARGET_CONFIG_PATH}"
  if [ "$CONFIG_MARKER_EXISTS" -eq 1 ]; then
    if ! run_privileged test -f "$TARGET_CONFIG_MARKER" || run_privileged test -L "$TARGET_CONFIG_MARKER"; then
      die "config was installed, but its existing ownership marker changed: ${TARGET_CONFIG_MARKER}"
    fi
  elif ! create_exclusive_with_text "$TARGET_CONFIG_MARKER" $'vmflow\n'; then
    die "config was installed and left in place, but the ownership marker could not be created safely: ${TARGET_CONFIG_MARKER}"
  fi
fi

log "Installed ${BINARY_NAME} ${VERSION} to ${TARGET_PATH}"
if [ "$INSTALL_CONFIG" -eq 1 ]; then
  log "Installed default config to ${TARGET_CONFIG_PATH}"
else
  log "Preserved existing config at ${TARGET_CONFIG_PATH}"
fi

configure_user_path "$INSTALL_DIR"
print_path_hint "$INSTALL_DIR" "$TARGET_PATH" "$TARGET_CONFIG_PATH"
if [ "$PRINT_INSTALL_DIR" -eq 1 ]; then
  printf '%s\n' "$INSTALL_DIR"
fi
