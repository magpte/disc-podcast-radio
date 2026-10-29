#!/bin/sh
unset LD_PRELOAD
set -eu
umask 077
cd "$(dirname "$0")"

# Safe mode immediate bypass: if DISC_MODULES_SAFE_MODE is present on TF card, exit immediately
if [ -f /tmp/sdcard/DISC_MODULES_SAFE_MODE ]; then
  echo "[disc-podcast-radio] DISC_MODULES_SAFE_MODE active; skipping startup." >/tmp/sdcard/disc-podcast-radio-startup.log 2>&1 || true
  exit 0
fi

# Clean up any stale/orphan daemon listening on port 8888
for p in $(pidof disc-podcast-radio 2>/dev/null || true); do
  if [ "$p" != "$$" ]; then
    kill -9 "$p" 2>/dev/null || true
  fi
done

data_dir=/usr/data/disc-runtime/module-data/disc-podcast-radio
mkdir -p "$data_dir" /tmp/disc-podcast-radio /tmp/disc-online-music 2>/dev/null || true
chmod 777 /tmp/disc-podcast-radio /tmp/disc-online-music 2>/dev/null || true

# Auto-prune old releases if count >= 3 to prevent componentctl.sh 3-version limit lockup
rel_dir="/usr/data/disc-runtime/components/disc-podcast-radio/releases"
if [ -d "$rel_dir" ]; then
  curr_link=$(readlink /usr/data/disc-runtime/components/disc-podcast-radio/current 2>/dev/null || echo "")
  curr_ver="${curr_link##*/}"
  rel_count=$(find "$rel_dir" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l || echo 0)
  if [ "$rel_count" -ge 3 ]; then
    for old_r in "$rel_dir"/*; do
      if [ -d "$old_r" ]; then
        ver_name="${old_r##*/}"
        if [ "$ver_name" != "$curr_ver" ]; then
          rm -rf "$old_r" 2>/dev/null || true
          rel_count=$((rel_count - 1))
          [ "$rel_count" -lt 2 ] && break
        fi
      fi
    done
  fi
fi

# Keep a diagnostic startup log on the TF card.
log_file=/tmp/sdcard/disc-podcast-radio-startup.log
if ! touch "$log_file" 2>/dev/null; then
  log_file="$data_dir/startup.log"
  touch "$log_file" 2>/dev/null || true
fi
[ -w "$log_file" ] || log_file=/dev/null
: >"$log_file" 2>/dev/null || true

console_log=/tmp/disc-runtime/component-disc-podcast-radio.log
mkdir -p /tmp/disc-runtime 2>/dev/null || true
if ! touch "$console_log" 2>/dev/null; then
  console_log=/tmp/disc-podcast-radio-console.log
  touch "$console_log" 2>/dev/null || true
fi
[ -w "$console_log" ] || console_log=/dev/null

# Check for updated binary on TF card to auto-execute and hot-upgrade
target_bin="./disc-podcast-radio"
tf_bin=""
for cand in /tmp/sdcard/DISC_MODULES/disc-podcast-radio/*/disc-podcast-radio /tmp/sdcard/disc-podcast-radio; do
  if [ -f "$cand" ] && [ -x "$cand" ]; then
    tf_bin="$cand"
  fi
done

if [ -n "$tf_bin" ]; then
  tf_hash=$(sha256sum "$tf_bin" 2>/dev/null | awk '{print $1}' || echo "tf")
  cur_hash=$(sha256sum "$target_bin" 2>/dev/null | awk '{print $1}' || echo "cur")
  tf_size=$(wc -c < "$tf_bin" 2>/dev/null || echo 0)
  cur_size=$(wc -c < "$target_bin" 2>/dev/null || echo 0)
  if [ "$tf_size" -gt 0 ] && { [ "$tf_hash" != "$cur_hash" ] || [ "$tf_size" -ne "$cur_size" ]; }; then
    echo "Found newer/updated binary on TF card ($tf_bin, hash=$tf_hash vs local=$cur_hash, size=$tf_size vs $cur_size)" >>"$log_file" 2>&1 || true
    # Attempt in-place update if local binary directory is writable
    if cp -f "$tf_bin" "./disc-podcast-radio.new" 2>/dev/null && chmod 755 "./disc-podcast-radio.new" 2>/dev/null; then
      mv -f "./disc-podcast-radio.new" "./disc-podcast-radio" 2>/dev/null || true
      echo "Local binary successfully updated from TF card." >>"$log_file" 2>&1 || true
    else
      target_bin="$tf_bin"
      echo "Local binary directory not writable, directly executing from TF card: $target_bin" >>"$log_file" 2>&1 || true
    fi
  fi
fi


{
  printf '\n=== %s disc-podcast-radio startup ===\n' "$(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo unknown)"
  printf 'pwd=%s\n' "$(pwd 2>/dev/null || echo unknown)"
  printf 'module_version=%s\n' "$(cat MODULE_VERSION 2>/dev/null || echo unknown)"
  printf 'executing_binary=%s\n' "$target_bin"
  uname -a 2>/dev/null || true
  ls -l "$target_bin" 2>/dev/null || true
  if sha256sum -c SHA256SUMS >/dev/null 2>&1; then
    echo "checksum: OK"
  else
    echo "checksum: FAILED or MISMATCH"
    sha256sum -c SHA256SUMS 2>&1 || true
  fi
  echo "--- network interfaces ---"
  ifconfig -a 2>/dev/null || ip addr 2>/dev/null || true
  echo "--- mounts ---"
  cat /proc/mounts 2>/dev/null || true
  echo "--- running processes ---"
  ps 2>/dev/null || true
} >>"$log_file" 2>&1 || true

# Ensure executable bit on target binary
chmod 755 "$target_bin" 2>/dev/null || true

# --- Start daemon ---
echo "starting: $target_bin -port 8888 -data $data_dir" >>"$log_file" 2>&1 || true

# Dual-pipe to console_log and TF card if available
tf_log=/tmp/sdcard/disc-podcast-radio.log
touch "$tf_log" 2>/dev/null || true

if [ -w "$tf_log" ]; then
  exec "$target_bin" -port 8888 -data "$data_dir" 2>&1 | tee -a "$tf_log" "$console_log"
else
  exec "$target_bin" -port 8888 -data "$data_dir" >>"$console_log" 2>&1
fi
