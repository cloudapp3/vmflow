#!/usr/bin/env bash
set -euo pipefail

REPO="cloudapp3/vmflow"
BINARY_NAME="vmflow"
MANAGED_MARKER_NAME=".vmflow-managed"
INSTALL_DIR="${VMFLOW_INSTALL_DIR:-}"
INSTALL_DIR_EXPLICIT=0
INSTALL_DIR_FROM_ENV=0
VERSION=""
SKIP_VERIFY=0
UNINSTALL=0
SYSTEM_INSTALL=0
NO_MODIFY_PATH="${VMFLOW_NO_MODIFY_PATH:-0}"
PRINT_INSTALL_DIR=0
USE_SUDO=0
SUDO_BIN="${VMFLOW_SUDO:-}"
PATH_RC=""
PATH_RC_ADDED=0
PATH_BLOCKS_REMOVED=0
REUSING_INSTALL=0
ARCHIVE_URL_OVERRIDE="${VMFLOW_ARCHIVE_URL:-}"
CHECKSUMS_URL_OVERRIDE="${VMFLOW_CHECKSUMS_URL:-}"
DOWNLOAD_HEADER_1="${VMFLOW_DOWNLOAD_HEADER_1:-}"
DOWNLOAD_HEADER_2="${VMFLOW_DOWNLOAD_HEADER_2:-}"
CUSTOM_RELEASE_SOURCE=0

if [ -n "$INSTALL_DIR" ]; then
  INSTALL_DIR_EXPLICIT=1
  INSTALL_DIR_FROM_ENV=1
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
  --skip-verify     Skip SHA-256 checksum verification.
  --no-modify-path  Do not add an automatic user install to a shell startup file.
  --print-install-dir
                    Print the selected install directory to stdout after success.
  --uninstall       Uninstall vmflow instead of installing. Stops the service
                    and removes the binary plus vmflow-managed data.
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

validate_option_value() {
  local option="$1"
  local value="$2"

  [ -n "$value" ] || die "empty value for ${option}"
  case "$value" in
    -*) die "invalid value for ${option}: values must not start with '-': ${value}" ;;
  esac
}

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

  # Reuse an existing binary in a conventional install directory even when the
  # current shell has not loaded that directory into PATH.
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
    PATH_RC_ADDED=1
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

  if [ "$USE_SUDO" -eq 1 ]; then
    log "Verify the root-owned system installation with:"
    log "  \"${SUDO_BIN}\" \"${target_path}\" version"
  elif path_has_dir "$install_dir"; then
    log "Verify the installation with:"
    log "  ${BINARY_NAME} version"
  else
    if [ -n "$PATH_RC" ]; then
      if [ "$PATH_RC_ADDED" -eq 1 ]; then
        log "Added ${install_dir} to ${PATH_RC}."
      else
        log "${install_dir} is already configured in ${PATH_RC}."
      fi
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
  log "Initialize vmflow and choose system-service or foreground mode:"
  log "  \"${target_path}\" init"
}

managed_config_dir_owned() {
  local dir="$1"
  local marker="${dir}/${MANAGED_MARKER_NAME}"
  local owner=""

  [ -d "$dir" ] && [ ! -L "$dir" ] \
    && [ -f "$marker" ] && [ ! -L "$marker" ] || return 1
  IFS= read -r owner <"$marker" || true
  [ "$owner" = "vmflow" ]
}

remove_managed_config_dir() {
  local dir="$1"
  local path

  managed_config_dir_owned "$dir" || return 1
  for path in \
    "$dir/config.yaml" \
    "$dir/client.yaml" \
    "$dir/stats.json"; do
    if [ -d "$path" ] && [ ! -L "$path" ]; then
      log "warning: refusing to remove directory at managed file path: $path"
      return 1
    fi
    rm -f -- "$path" || return 1
  done
  rm -f -- "$dir/$MANAGED_MARKER_NAME" || return 1
  if ! rmdir "$dir" 2>/dev/null; then
    log "Preserved remaining non-vmflow files in managed config directory: $dir"
  fi
}

systemd_state_is_stopped() {
  case "$1" in
    inactive|failed|unknown) return 0 ;;
    *) return 1 ;;
  esac
}

SYSTEMD_STATE=""
SYSTEMD_STATE_DETAIL=""
probe_systemd_state() {
  local output
  local status=0

  output="$(systemctl is-active "$BINARY_NAME" 2>&1)" || status=$?
  case "$output" in
    active|reloading|inactive|failed|activating|deactivating|maintenance|refreshing|unknown)
      SYSTEMD_STATE="$output"
      SYSTEMD_STATE_DETAIL=""
      return 0
      ;;
  esac
  SYSTEMD_STATE=""
  SYSTEMD_STATE_DETAIL="${output:-exit status ${status}}"
  return 1
}

stop_systemd_service() {
  local unit="/etc/systemd/system/${BINARY_NAME}.service"
  local has_unit=0
  local output

  if [ -e "$unit" ] || [ -L "$unit" ]; then
    has_unit=1
  fi
  if ! command -v systemctl >/dev/null 2>&1; then
    if [ "$has_unit" -eq 1 ]; then
      log "Error: cannot stop ${BINARY_NAME}: systemctl is unavailable"
      return 1
    fi
    return 0
  fi
  if ! probe_systemd_state; then
    if [ "$has_unit" -eq 0 ]; then
      return 0
    fi
    log "Error: could not determine systemd state for ${BINARY_NAME}: ${SYSTEMD_STATE_DETAIL}"
    return 1
  fi
  if [ "$has_unit" -eq 0 ] && systemd_state_is_stopped "$SYSTEMD_STATE"; then
    return 0
  fi

  if ! systemd_state_is_stopped "$SYSTEMD_STATE"; then
    if ! output="$(systemctl stop "$BINARY_NAME" 2>&1)"; then
      log "Error: could not stop systemd service ${BINARY_NAME}${output:+: ${output}}"
      return 1
    fi
    if ! probe_systemd_state; then
      log "Error: could not verify systemd service ${BINARY_NAME} stopped: ${SYSTEMD_STATE_DETAIL}"
      return 1
    fi
    if ! systemd_state_is_stopped "$SYSTEMD_STATE"; then
      log "Error: systemd service ${BINARY_NAME} is still ${SYSTEMD_STATE} after stop"
      return 1
    fi
  fi

  if ! output="$(systemctl disable "$BINARY_NAME" 2>&1)"; then
    log "Error: could not disable systemd service ${BINARY_NAME}${output:+: ${output}}"
    return 1
  fi
  if ! rm -f -- "$unit"; then
    log "Error: could not remove systemd unit ${unit}"
    return 1
  fi
  if ! output="$(systemctl daemon-reload 2>&1)"; then
    log "Error: systemctl daemon-reload failed${output:+: ${output}}"
    return 1
  fi
}

LAUNCHD_LOADED=0
LAUNCHD_DETAIL=""
probe_launchd_state() {
  local label="$1"
  local output
  local status=0

  output="$(launchctl print "system/${label}" 2>&1)" || status=$?
  if [ "$status" -eq 0 ]; then
    LAUNCHD_LOADED=1
    LAUNCHD_DETAIL=""
    return 0
  fi
  case "$output" in
    *"Could not find service"*|*"could not find service"*|*"Service not found"*|*"service not found"*)
      LAUNCHD_LOADED=0
      LAUNCHD_DETAIL=""
      return 0
      ;;
  esac
  if [ "$status" -eq 113 ]; then
    LAUNCHD_LOADED=0
    LAUNCHD_DETAIL=""
    return 0
  fi
  LAUNCHD_LOADED=0
  LAUNCHD_DETAIL="${output:-exit status ${status}}"
  return 1
}

stop_launchd_service() {
  local label="io.cloudapp.${BINARY_NAME}"
  local plist="/Library/LaunchDaemons/${label}.plist"
  local has_plist=0
  local output

  if [ -e "$plist" ] || [ -L "$plist" ]; then
    has_plist=1
  fi
  if ! command -v launchctl >/dev/null 2>&1; then
    if [ "$has_plist" -eq 1 ]; then
      log "Error: cannot stop ${BINARY_NAME}: launchctl is unavailable"
      return 1
    fi
    return 0
  fi
  if ! probe_launchd_state "$label"; then
    if [ "$has_plist" -eq 0 ]; then
      return 0
    fi
    log "Error: could not determine launchd state for ${label}: ${LAUNCHD_DETAIL}"
    return 1
  fi
  if [ "$LAUNCHD_LOADED" -eq 1 ]; then
    if ! output="$(launchctl bootout "system/${label}" 2>&1)"; then
      log "Error: could not stop launchd daemon ${label}${output:+: ${output}}"
      return 1
    fi
    if ! probe_launchd_state "$label"; then
      log "Error: could not verify launchd daemon ${label} stopped: ${LAUNCHD_DETAIL}"
      return 1
    fi
    if [ "$LAUNCHD_LOADED" -eq 1 ]; then
      log "Error: launchd daemon ${label} is still loaded after bootout"
      return 1
    fi
  fi
  if ! rm -f -- "$plist"; then
    log "Error: could not remove launchd plist ${plist}"
    return 1
  fi
}

# stop_service stops and verifies the native service before removing its
# definition. Windows is not supported by install.sh.
stop_service() {
  case "$(uname -s)" in
    Linux) stop_systemd_service ;;
    Darwin) stop_launchd_service ;;
    *) return 0 ;;
  esac
}

remove_path_block_from_file() {
  local rc_file="$1"
  local install_dir="$2"
  local quoted_install_dir
  local path_line
  local temp_file
  local awk_status

  [ -e "$rc_file" ] || [ -L "$rc_file" ] || return 0
  if [ -L "$rc_file" ] || [ ! -f "$rc_file" ]; then
    log "Warning: cannot clean vmflow PATH entry because ${rc_file} is not a regular file."
    return 0
  fi
  if [ ! -w "$rc_file" ]; then
    log "Warning: cannot clean vmflow PATH entry because ${rc_file} is not writable."
    return 0
  fi
  if ! command -v awk >/dev/null 2>&1 || ! command -v mktemp >/dev/null 2>&1; then
    log "Warning: cannot clean vmflow PATH entry in ${rc_file} because awk or mktemp is unavailable."
    return 0
  fi

  printf -v quoted_install_dir '%q' "$install_dir"
  path_line="export PATH=${quoted_install_dir}:\$PATH"
  if ! grep -Fqx '# vmflow user install' "$rc_file" 2>/dev/null; then
    return 0
  fi

  if ! temp_file="$(mktemp "${TMPDIR:-/tmp}/vmflow-path.XXXXXX")"; then
    log "Warning: could not create a temporary file while cleaning ${rc_file}."
    return 0
  fi

  awk_status=0
  VMFLOW_PATH_LINE="$path_line" awk '
    BEGIN {
      marker = "# vmflow user install"
      path_line = ENVIRON["VMFLOW_PATH_LINE"]
    }
    pending {
      if ($0 == path_line) {
        removed = 1
        pending = 0
        next
      }
      print marker
      pending = 0
    }
    $0 == marker {
      pending = 1
      next
    }
    { print }
    END {
      if (pending) {
        print marker
      }
      if (removed) {
        exit 42
      }
    }
  ' "$rc_file" >"$temp_file" || awk_status=$?

  if [ "$awk_status" -eq 0 ]; then
    rm -f "$temp_file"
    return 0
  fi
  if [ "$awk_status" -ne 42 ]; then
    log "Warning: could not parse ${rc_file} while cleaning the vmflow PATH entry."
    rm -f "$temp_file"
    return 0
  fi

  # Revalidate immediately before truncating the existing file. Writing through
  # the file preserves its owner and mode instead of replacing it with mktemp's.
  if [ -L "$rc_file" ] || [ ! -f "$rc_file" ] || [ ! -w "$rc_file" ]; then
    log "Warning: ${rc_file} changed while cleaning the vmflow PATH entry; leaving it untouched."
    rm -f "$temp_file"
    return 0
  fi
  if ! cat "$temp_file" >"$rc_file"; then
    log "Warning: could not remove the vmflow PATH entry from ${rc_file}."
    rm -f "$temp_file"
    return 0
  fi

  rm -f "$temp_file"
  PATH_BLOCKS_REMOVED=$((PATH_BLOCKS_REMOVED + 1))
  log "Removed vmflow PATH entry from ${rc_file}."
}

remove_user_path_blocks() {
  local requested_dir="${1:-}"
  local rc_file
  local install_dir
  local -a install_dirs

  if [ -z "${HOME:-}" ]; then
    log "Warning: HOME is unset; shell PATH entries were not cleaned."
    return 0
  fi

  if [ -n "$requested_dir" ]; then
    case "$requested_dir" in
      "$HOME/.local/bin"|"$HOME/bin") install_dirs=("$requested_dir") ;;
      *) return 0 ;;
    esac
  else
    install_dirs=("$HOME/.local/bin" "$HOME/bin")
  fi

  for install_dir in "${install_dirs[@]}"; do
    if [ -e "$install_dir/$BINARY_NAME" ] || [ -L "$install_dir/$BINARY_NAME" ]; then
      log "Preserving vmflow PATH entry because ${install_dir}/${BINARY_NAME} still exists."
      continue
    fi
    for rc_file in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile"; do
      remove_path_block_from_file "$rc_file" "$install_dir"
    done
  done
}

run_delegated_uninstall() {
  local target="$1"
  local status

  if [ -t 0 ]; then
    "$target" uninstall
    return $?
  fi

  if { exec 3</dev/tty; } 2>/dev/null; then
    status=0
    "$target" uninstall <&3 || status=$?
    exec 3<&-
    return "$status"
  fi

  log "No controlling terminal is available; delegated uninstall will use non-interactive input."
  "$target" uninstall </dev/null
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
  PATH_CLEANUP_DIR=""
  if [ -n "$TARGET" ]; then
    PATH_CLEANUP_DIR="$(dirname "$TARGET")"
  fi

  # Delegate to the binary's own uninstall when available (richest cleanup).
  if [ -n "$TARGET" ] && [ -x "$TARGET" ] && "$TARGET" uninstall --help >/dev/null 2>&1; then
    log "Delegating to: $TARGET uninstall"
    uninstall_status=0
    run_delegated_uninstall "$TARGET" || uninstall_status=$?
    [ "$uninstall_status" -eq 0 ] \
      || die "delegated uninstall failed with status ${uninstall_status}"
    if [ -e "$TARGET" ] || [ -L "$TARGET" ]; then
      log "Delegated uninstall left ${TARGET} in place; shell PATH entries were not changed."
      return 0
    fi
    remove_user_path_blocks "$PATH_CLEANUP_DIR"
    return 0
  fi

  log "vmflow binary not found (or too old for self-uninstall) at ${TARGET:-<none>}; cleaning up via shell."

  case "$(uname -s)" in
    Linux)
      SYSTEM_CONFIG_DIR="/etc/vmflow"
      USER_CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/vmflow"
      ;;
    Darwin)
      SYSTEM_CONFIG_DIR="/Library/Application Support/vmflow"
      USER_CONFIG_DIR="$HOME/Library/Application Support/vmflow"
      ;;
    *)
      SYSTEM_CONFIG_DIR=""
      USER_CONFIG_DIR=""
      ;;
  esac
  CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/vmflow"
  CACHE_FILE="${CACHE_DIR}/update-check.json"

  # Build the removal plan (only entries that exist).
  PLAN=()
  if [ -n "$TARGET" ] && [ -x "$TARGET" ]; then PLAN+=("$TARGET"); fi
  for config_dir in "$SYSTEM_CONFIG_DIR" "$USER_CONFIG_DIR"; do
    [ -n "$config_dir" ] || continue
    if managed_config_dir_owned "$config_dir"; then
      PLAN+=("$config_dir")
    elif [ -e "$config_dir/config.yaml" ] || [ -L "$config_dir/config.yaml" ]; then
      log "Preserving unowned config directory: $config_dir"
    fi
  done
  if [ -d /var/log/vmflow ]; then PLAN+=("/var/log/vmflow"); fi
  if [ -f "$CACHE_FILE" ]; then PLAN+=("$CACHE_FILE"); fi

  if [ "${#PLAN[@]}" -eq 0 ]; then
    stop_service \
      || die "native service cleanup failed; vmflow binary, config, logs, and cache were left in place"
    remove_user_path_blocks "$PATH_CLEANUP_DIR"
    if [ "$PATH_BLOCKS_REMOVED" -eq 0 ]; then
      log "Nothing left to remove."
    else
      log "No vmflow files remained; stale shell PATH entries were removed."
    fi
    return 0
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

  stop_service \
    || die "native service cleanup failed; vmflow binary, config, logs, and cache were left in place"

  for t in "${PLAN[@]}"; do
    if { [ -n "$SYSTEM_CONFIG_DIR" ] && [ "$t" = "$SYSTEM_CONFIG_DIR" ]; } \
      || { [ -n "$USER_CONFIG_DIR" ] && [ "$t" = "$USER_CONFIG_DIR" ]; }; then
      if ! managed_config_dir_owned "$t"; then
        log "warning: preserving config directory because its ownership marker changed: $t"
        continue
      fi
      if remove_managed_config_dir "$t"; then
        log "removed vmflow-managed config files from $t"
      else
        log "warning: could not safely remove vmflow-managed config files from $t"
      fi
      continue
    elif [ "$t" = "/var/log/vmflow" ]; then
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

  remove_user_path_blocks "$PATH_CLEANUP_DIR"

  log "vmflow uninstalled."
  log "External TLS certificate and key files referenced by the config are preserved."
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      shift
      [ "$#" -gt 0 ] || die "missing value for --version"
      validate_option_value "--version" "$1"
      VERSION="$1"
      ;;
    --version=*)
      VERSION="${1#*=}"
      validate_option_value "--version" "$VERSION"
      ;;
    --dir)
      shift
      [ "$#" -gt 0 ] || die "missing value for --dir"
      INSTALL_DIR="$1"
      validate_option_value "--dir" "$INSTALL_DIR"
      INSTALL_DIR_EXPLICIT=1
      INSTALL_DIR_FROM_ENV=0
      ;;
    --dir=*)
      INSTALL_DIR="${1#*=}"
      validate_option_value "--dir" "$INSTALL_DIR"
      INSTALL_DIR_EXPLICIT=1
      INSTALL_DIR_FROM_ENV=0
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

if [ "$INSTALL_DIR_FROM_ENV" -eq 1 ]; then
  validate_option_value "VMFLOW_INSTALL_DIR" "$INSTALL_DIR"
fi

if [ "$UNINSTALL" -eq 1 ]; then
  do_uninstall
  exit 0
fi

if [ -n "$ARCHIVE_URL_OVERRIDE" ] || [ -n "$CHECKSUMS_URL_OVERRIDE" ]; then
  [ -n "$ARCHIVE_URL_OVERRIDE" ] && [ -n "$CHECKSUMS_URL_OVERRIDE" ] \
    || die "VMFLOW_ARCHIVE_URL and VMFLOW_CHECKSUMS_URL must be set together"
  [ -n "$VERSION" ] \
    || die "--version is required with a custom release source"
  for release_url in "$ARCHIVE_URL_OVERRIDE" "$CHECKSUMS_URL_OVERRIDE"; do
    case "$release_url" in
      http://*|https://*) ;;
      *) die "custom release URLs must start with http:// or https://" ;;
    esac
  done
  CUSTOM_RELEASE_SOURCE=1
elif [ -n "$DOWNLOAD_HEADER_1" ] || [ -n "$DOWNLOAD_HEADER_2" ]; then
  die "custom download headers require VMFLOW_ARCHIVE_URL and VMFLOW_CHECKSUMS_URL"
fi

for download_header in "$DOWNLOAD_HEADER_1" "$DOWNLOAD_HEADER_2"; do
  [ -z "$download_header" ] && continue
  case "$download_header" in
    *$'\r'*|*$'\n'*) die "custom download headers must be single-line values" ;;
    *:*) ;;
    *) die "custom download headers must use 'Name: value' syntax" ;;
  esac
done

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
DOWNLOAD_CURL_ARGS=("${CURL_ARGS[@]}")
if [ "$CUSTOM_RELEASE_SOURCE" -eq 1 ]; then
  # Never forward a GitHub token to an explicitly configured download host.
  DOWNLOAD_CURL_ARGS=(-fsSL)
  [ -z "$DOWNLOAD_HEADER_1" ] || DOWNLOAD_CURL_ARGS+=(-H "$DOWNLOAD_HEADER_1")
  [ -z "$DOWNLOAD_HEADER_2" ] || DOWNLOAD_CURL_ARGS+=(-H "$DOWNLOAD_HEADER_2")
fi

curl_text() {
  curl "${CURL_ARGS[@]}" "$1"
}

curl_download() {
  local url="$1"
  local output="$2"
  curl "${DOWNLOAD_CURL_ARGS[@]}" -o "$output" "$url"
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
if [ "$CUSTOM_RELEASE_SOURCE" -eq 1 ]; then
  ARCHIVE_URL="$ARCHIVE_URL_OVERRIDE"
  CHECKSUMS_URL="$CHECKSUMS_URL_OVERRIDE"
else
  ARCHIVE_URL="${BASE_URL}/${ARCHIVE_NAME}"
  CHECKSUMS_URL="${BASE_URL}/checksums.txt"
fi

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

if ! run_privileged test -d "$INSTALL_DIR"; then
  run_privileged mkdir -p "$INSTALL_DIR" 2>/dev/null \
    || die "cannot create install directory: ${INSTALL_DIR}"
fi
run_privileged test -w "$INSTALL_DIR" \
  || die "install directory is not writable: ${INSTALL_DIR}; use --system for a privileged install"

TARGET_PATH="${INSTALL_DIR}/${BINARY_NAME}"
if run_privileged test -L "$TARGET_PATH"; then
  die "existing binary path is a symbolic link: ${TARGET_PATH}"
fi
if run_privileged test -e "$TARGET_PATH" && ! run_privileged test -f "$TARGET_PATH"; then
  die "existing binary path is not a regular file: ${TARGET_PATH}"
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

log "Installed ${BINARY_NAME} ${VERSION} to ${TARGET_PATH}"

configure_user_path "$INSTALL_DIR"
print_path_hint "$INSTALL_DIR" "$TARGET_PATH"
if [ "$PRINT_INSTALL_DIR" -eq 1 ]; then
  printf '%s\n' "$INSTALL_DIR"
fi
