#!/usr/bin/env bash

set -euo pipefail

readonly SERVICE_NAME="ewelink-lan-ctl.service"
readonly INSTALL_PATH="/usr/local/bin/ewelink-lan-ctl"
readonly SERVICE_PATH="/etc/systemd/system/ewelink-lan-ctl.service"
readonly CONFIG_PATH="/etc/ewelink-lan-ctl.env"
readonly STATE_DIR="/var/lib/ewelink-lan-ctl"

purge=false

usage() {
  cat <<'EOF'
用法：
  sudo ./uninstall.sh [--purge]

默认仅移除程序和 systemd 服务，保留配置及 OAuth 状态，方便以后重装。
使用 --purge 会额外永久删除配置、OAuth 状态和专用系统用户。
EOF
}

fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

while (($# > 0)); do
  case "$1" in
    --purge)
      purge=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "未知选项：$1"
      ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || fail "本脚本仅支持 Linux"
[[ "$(id -u)" -eq 0 ]] || fail "请使用 sudo 或 root 运行本脚本"
command -v systemctl >/dev/null 2>&1 || fail "缺少必要命令：systemctl"

systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
rm -f -- "$SERVICE_PATH" "$INSTALL_PATH" "${INSTALL_PATH}.new"
systemctl daemon-reload
systemctl reset-failed "$SERVICE_NAME" 2>/dev/null || true

if [[ "$purge" == true ]]; then
  rm -f -- "$CONFIG_PATH"
  rm -rf -- "$STATE_DIR"
  if id -u ewelink >/dev/null 2>&1; then
    userdel ewelink
  fi
  if getent group ewelink >/dev/null 2>&1; then
    groupdel ewelink
  fi
  printf '%s 已彻底卸载，配置和 OAuth 状态已删除。\n' "${SERVICE_NAME%.service}"
else
  printf '%s 已卸载。\n' "${SERVICE_NAME%.service}"
  printf '已保留配置：%s\n' "$CONFIG_PATH"
  printf '已保留 OAuth 状态：%s\n' "$STATE_DIR"
  printf '如需彻底清理，请运行：sudo ./uninstall.sh --purge\n'
fi
