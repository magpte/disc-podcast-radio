#!/bin/sh
unset LD_PRELOAD
set -eu
umask 077
cd "$(dirname "$0")"

data_dir=/usr/data/disc-runtime/module-data/disc-podcast-radio
mkdir -p "$data_dir" 2>/dev/null || true

# Keep a short, bounded startup diagnostic on the TF card.
log_file=/tmp/sdcard/disc-podcast-radio-startup.log
if ! touch "$log_file" 2>/dev/null; then
  log_file="$data_dir/startup.log"
  touch "$log_file" 2>/dev/null || true
fi
[ -w "$log_file" ] || log_file=/dev/null
: >"$log_file" 2>/dev/null || true

console_log=/tmp/disc-runtime/component-disc-podcast-radio.log
if ! touch "$console_log" 2>/dev/null; then
  console_log=/tmp/disc-podcast-radio-console.log
  touch "$console_log" 2>/dev/null || true
fi
[ -w "$console_log" ] || console_log=/dev/null

{
  printf '\n=== %s disc-podcast-radio startup ===\n' "$(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo unknown)"
  printf 'pwd=%s\n' "$(pwd 2>/dev/null || echo unknown)"
  printf 'module_version=%s\n' "$(cat MODULE_VERSION 2>/dev/null || echo unknown)"
  uname -a 2>/dev/null || true
  ls -l ./disc-podcast-radio 2>/dev/null || true
  if sha256sum -c SHA256SUMS >/dev/null 2>&1; then
    echo "checksum: OK"
  else
    echo "checksum: FAILED"
    sha256sum -c SHA256SUMS 2>&1 || true
  fi
} >>"$log_file" 2>&1 || true

log() { printf '[disc-podcast-radio] %s\n' "$*" >>"$log_file" 2>/dev/null || true; }
sha() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }

stock_sha=7cfe223719fa5d53d169c6fbd303e2123d4f99b14bca62262f4875c826e2bb28
seed_dir=/usr/project/disc-system-v1
target_so=/usr/data/disc-settings/module-ui.so
orig_so=/usr/data/disc-settings/module-ui-orig.so
ui_backup="$data_dir/ui-backup"
bt_component=/usr/data/disc-runtime/components/disc-bt-meter
bt_loader=/usr/data/lib/librt.so.1
bridge_rel=bridge/disc-settings.so

# Replace the checksum of one file in a SHA256SUMS file. Runs in the same
# directory so the final rename stays atomic.
set_sums_hash() {
  f=$1; p=$2; h=$3; tmp=$4
  awk -v h="$h" -v p="$p" \
    '$2==p{print h"  "$2; found=1; next} {print} END{if(!found) print h"  "p}' \
    "$f" >"$tmp" 2>/dev/null || return 1
  mv -f "$tmp" "$f" 2>/dev/null || { rm -f "$tmp" 2>/dev/null; return 1; }
  return 0
}

# The installed bt-meter release, but only the persistent userdata copy.
bt_release() {
  r=$(readlink -f "$bt_component/current" 2>/dev/null || true)
  case "$r" in
    /usr/data/disc-runtime/components/disc-bt-meter/releases/*) printf '%s\n' "$r" ;;
  esac
}

# Print the path of a verified stock module. The seed copy is authoritative and
# is identical to the stock bt-meter bridge (hash 7cfe2237...).
stock_source() {
  if [ -f "$ui_backup/module-ui.so" ] && [ "$(sha "$ui_backup/module-ui.so")" = "$stock_sha" ]; then
    printf '%s\n' "$ui_backup/module-ui.so"; return 0
  fi
  if [ -f "$orig_so" ] && [ "$(sha "$orig_so")" = "$stock_sha" ]; then
    printf '%s\n' "$orig_so"; return 0
  fi
  if [ -f "$seed_dir/disc-settings.so" ]; then
    printf '%s\n' "$seed_dir/disc-settings.so"; return 0
  fi
  return 1
}

ensure_bridge_backup() {
  b=$1
  mkdir -p "$ui_backup" 2>/dev/null || true
  if [ -f "$ui_backup/disc-settings.so" ] && [ "$(sha "$ui_backup/disc-settings.so")" = "$stock_sha" ]; then
    return 0
  fi
  if [ "$(sha "$b")" = "$stock_sha" ]; then
    cp -f "$b" "$ui_backup/disc-settings.so" 2>/dev/null && return 0
  fi
  if [ -f "$seed_dir/disc-settings.so" ] && [ "$(sha "$seed_dir/disc-settings.so")" = "$stock_sha" ]; then
    cp -f "$seed_dir/disc-settings.so" "$ui_backup/disc-settings.so" 2>/dev/null && return 0
  fi
  return 1
}

ensure_module_backup() {
  cur=$1
  mkdir -p "$ui_backup" 2>/dev/null || true
  if [ -f "$ui_backup/module-ui.so" ] && [ "$(sha "$ui_backup/module-ui.so")" = "$stock_sha" ]; then
    return 0
  fi
  if [ "$cur" = "$stock_sha" ] && [ -f "$target_so" ]; then
    cp -f "$target_so" "$ui_backup/module-ui.so" 2>/dev/null && return 0
  fi
  if [ -f "$orig_so" ] && [ "$(sha "$orig_so")" = "$stock_sha" ]; then
    cp -f "$orig_so" "$ui_backup/module-ui.so" 2>/dev/null && return 0
  fi
  if [ -f "$seed_dir/disc-settings.so" ] && [ "$(sha "$seed_dir/disc-settings.so")" = "$stock_sha" ]; then
    cp -f "$seed_dir/disc-settings.so" "$ui_backup/module-ui.so" 2>/dev/null && return 0
  fi
  return 1
}

restore_bridge() {
  rel=$1
  b=$rel/$bridge_rel
  s=$rel/SHA256SUMS
  src=$(stock_source || true)
  ok=0
  if [ -n "${src:-}" ]; then
    if cp -f "$src" "$b.next" 2>/dev/null && chmod 644 "$b.next" 2>/dev/null && mv -f "$b.next" "$b" 2>/dev/null; then
      ok=1
      log "restored stock bt-meter bridge"
    else
      rm -f "$b.next" 2>/dev/null || true
    fi
  fi
  if [ "$ok" = 1 ]; then
    set_sums_hash "$s" "$bridge_rel" "$stock_sha" "$s.next.$$" || true
  fi
  sync 2>/dev/null || true
  [ "$ok" = 1 ]
}

revert_ui() {
  log "revert requested"
  rel=$(bt_release || true)
  if [ -n "${rel:-}" ] && ! restore_bridge "$rel"; then
    log "could not restore bt-meter bridge; keeping current module-ui.so"
    return 0
  fi
  ok=0
  src=$(stock_source || true)
  if [ -n "${src:-}" ]; then
    if cp -f "$src" "$target_so.next" 2>/dev/null && chmod 644 "$target_so.next" 2>/dev/null && mv -f "$target_so.next" "$target_so" 2>/dev/null; then
      log "restored stock module-ui.so"
      ok=1
    else
      rm -f "$target_so.next" 2>/dev/null || true
      log "failed to restore module-ui.so"
    fi
  else
    log "no stock module-ui.so available to restore"
  fi
  sync 2>/dev/null || true
  ui_pid=$(pidof mq_ui 2>/dev/null || true)
  [ -z "$ui_pid" ] || kill -TERM $ui_pid 2>/dev/null || true
  if [ "$ok" = 1 ]; then
    : >/tmp/sdcard/DISABLE_UI_HOOK 2>/dev/null || log "WARNING: could not create DISABLE_UI_HOOK"
    log "UI module disabled until DISABLE_UI_HOOK is removed"
    rm -f /tmp/sdcard/REVERT_UI ./REVERT_UI 2>/dev/null || true
  fi
  return 0
}

deploy_ui() {
  [ -f ./module-ui.so ] || return 0
  [ -f ./test_loader ] || { log "test_loader missing; UI module not installed"; return 0; }
  [ -f /tmp/sdcard/DISABLE_UI_HOOK ] && { log "UI module disabled by DISABLE_UI_HOOK"; return 0; }
  [ -f /tmp/sdcard/ENABLE_UI_HOOK ] || [ -f ./ENABLE_UI_HOOK ] || return 0

  our_sha=$(sha ./module-ui.so)
  case "$our_sha" in ''|*[!0-9a-f]*) log "cannot hash package module-ui.so"; return 0;; esac

  rel=$(bt_release || true)
  if [ -z "${rel:-}" ]; then
    log "bt-meter release is not installed under /usr/data; UI module skipped"
    return 0
  fi
  sums=$rel/SHA256SUMS
  bridge=$rel/$bridge_rel
  [ -f "$sums" ] && [ -f "$bridge" ] || { log "bt-meter bridge files missing; UI module skipped"; return 0; }
  [ -f "$rel/bridge/librt-ui-loader.so.1" ] || { log "bt-meter loader missing; UI module skipped"; return 0; }

  loader_sha=$(sha "$bt_loader")
  rel_loader_sha=$(sha "$rel/bridge/librt-ui-loader.so.1")
  if [ -z "$loader_sha" ] || [ "$loader_sha" != "$rel_loader_sha" ]; then
    log "active UI loader does not match bt-meter release; UI module skipped"
    return 0
  fi
  if [ -f "$seed_dir/librt.so.1" ] && [ "$loader_sha" = "$(sha "$seed_dir/librt.so.1")" ]; then
    log "stock seed loader active; UI module skipped"
    return 0
  fi

  # Validate the packaged ELF before touching any system file.
  chmod 755 ./test_loader 2>/dev/null || true
  if ! ./test_loader "$PWD/module-ui.so" >>"$log_file" 2>&1; then
    log "test_loader failed; UI module not installed"
    return 0
  fi

  # A power loss can leave the bridge and its checksum line out of sync.
  # Repair only that one entry; refuse to touch any other broken file.
  if ! (cd "$rel" && sha256sum -c SHA256SUMS >/dev/null 2>&1); then
    actual_bridge=$(sha "$bridge")
    old_line=$(awk -v p="$bridge_rel" '$2==p{print $1; exit}' "$sums" 2>/dev/null || true)
    if [ -n "$actual_bridge" ] && [ -n "$old_line" ] && \
       set_sums_hash "$sums" "$bridge_rel" "$actual_bridge" "$sums.next.$$"; then
      if ! (cd "$rel" && sha256sum -c SHA256SUMS >/dev/null 2>&1); then
        set_sums_hash "$sums" "$bridge_rel" "$old_line" "$sums.next.$$" || true
        log "bt-meter release checksum failed beyond the bridge; UI module skipped"
        return 0
      fi
      log "repaired bt-meter bridge checksum entry"
    else
      log "bt-meter release checksum failed; UI module skipped"
      return 0
    fi
  fi

  bridge_changed=0
  if [ "$(sha "$bridge")" != "$our_sha" ]; then
    if [ ! -w "$bridge" ] || [ ! -w "$sums" ]; then
      log "bt-meter bridge is not writable; UI module skipped"
      return 0
    fi
    if ! ensure_bridge_backup "$bridge"; then
      log "no stock bridge copy available; UI module skipped"
      return 0
    fi
    if ! cp -f ./module-ui.so "$bridge.next" 2>/dev/null; then
      rm -f "$bridge.next" 2>/dev/null || true
      log "failed to stage bt-meter bridge"
      return 0
    fi
    chmod 644 "$bridge.next" 2>/dev/null || true
    if ! mv -f "$bridge.next" "$bridge" 2>/dev/null; then
      rm -f "$bridge.next" 2>/dev/null || true
      log "failed to install bt-meter bridge"
      return 0
    fi
    if ! set_sums_hash "$sums" "$bridge_rel" "$our_sha" "$sums.next.$$"; then
      restore_bridge "$rel" || true
      log "failed to update bt-meter SHA256SUMS; rolled back"
      return 0
    fi
    sync 2>/dev/null || true
    if ! (cd "$rel" && sha256sum -c SHA256SUMS >/dev/null 2>&1); then
      restore_bridge "$rel" || true
      log "bt-meter bridge verification failed; rolled back"
      return 0
    fi
    bridge_changed=1
    log "installed patched bt-meter bridge"
  fi

  module_changed=0
  cur_sha=$(sha "$target_so")
  if [ "$cur_sha" != "$our_sha" ]; then
    mkdir -p /usr/data/disc-settings 2>/dev/null || true
    ensure_module_backup "$cur_sha" || true
    if ! cp -f ./module-ui.so "$target_so.next" 2>/dev/null; then
      rm -f "$target_so.next" 2>/dev/null || true
      log "failed to stage module-ui.so"
      if [ "$bridge_changed" = 1 ]; then restore_bridge "$rel" || true; fi
      return 0
    fi
    chmod 644 "$target_so.next" 2>/dev/null || true
    if ! mv -f "$target_so.next" "$target_so" 2>/dev/null; then
      rm -f "$target_so.next" 2>/dev/null || true
      log "failed to install module-ui.so"
      if [ "$bridge_changed" = 1 ]; then restore_bridge "$rel" || true; fi
      return 0
    fi
    sync 2>/dev/null || true
    if [ "$(sha "$target_so")" != "$our_sha" ]; then
      log "module-ui.so verification failed"
      if [ "$bridge_changed" = 1 ]; then restore_bridge "$rel" || true; fi
      return 0
    fi
    module_changed=1
    log "installed patched module-ui.so"
  else
    log "module-ui.so already current"
  fi

  if [ "$(sha "$bridge")" != "$our_sha" ]; then
    log "WARNING: bt-meter bridge and module-ui.so hashes differ"
  fi

  if [ "$module_changed" = 1 ]; then
    ui_pid=$(pidof mq_ui 2>/dev/null || true)
    if [ -n "$ui_pid" ]; then
      log "restarting mq_ui to load the new module"
      kill -TERM $ui_pid 2>/dev/null || true
    fi
  fi
  return 0
}

revert=0
if [ -f /tmp/sdcard/REVERT_UI ] || [ -f ./REVERT_UI ]; then
  revert_ui || true
  revert=1
fi
if [ "$revert" = 0 ]; then
  deploy_ui || true
fi

# --- Start daemon ---
echo "starting: ./disc-podcast-radio -port 8888 -data $data_dir" >>"$log_file" 2>&1 || true
chmod 755 ./disc-podcast-radio 2>/dev/null || true
if ./disc-podcast-radio -port 8888 -data "$data_dir" >>"$console_log" 2>&1; then
  status=0
else
  status=$?
fi
{
  printf '%s disc-podcast-radio exited status=%s\n' "$(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo unknown)" "$status"
  if [ -s "$console_log" ]; then
    echo "--- last 64 KiB of $console_log ---"
    tail -c 65536 "$console_log" 2>/dev/null || true
    echo
    echo "--- end ---"
  fi
} >>"$log_file" 2>&1 || true
exit "$status"
