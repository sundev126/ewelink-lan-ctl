#!/usr/bin/env bash

set -euo pipefail
unset CDPATH

readonly REPOSITORY="sundev126/ewelink-lan-ctl"
readonly PROGRAM="ewelink-lan-ctl"
readonly SERVICE_NAME="ewelink-lan-ctl.service"
readonly INSTALL_PATH="/usr/local/bin/ewelink-lan-ctl"
readonly SERVICE_PATH="/etc/systemd/system/ewelink-lan-ctl.service"
readonly CONFIG_PATH="/etc/ewelink-lan-ctl.env"
readonly STATE_DIR="/var/lib/ewelink-lan-ctl"

version="latest"
config_source=""

usage() {
  cat <<'EOF'
用法：
  sudo ./install.sh [--version v1.0.0] [配置文件]

首次安装必须提供配置文件，例如：
  sudo ./install.sh ./.env

再次执行会升级程序，并保留现有配置和 OAuth 状态。若要替换已有配置，
请再次显式传入配置文件。
EOF
}

fail() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少必要命令：$1"
}

while (($# > 0)); do
  case "$1" in
    --version)
      (($# >= 2)) || fail "--version 后必须提供版本号"
      version="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --*)
      fail "未知选项：$1"
      ;;
    *)
      [[ -z "$config_source" ]] || fail "只能提供一个配置文件"
      config_source="$1"
      shift
      ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || fail "本脚本仅支持 Linux"
[[ "$(id -u)" -eq 0 ]] || fail "请使用 sudo 或 root 运行本脚本"
[[ -d /run/systemd/system ]] || fail "当前系统未使用 systemd"

case "$(uname -m)" in
  x86_64|amd64)
    architecture="amd64"
    ;;
  *)
    fail "当前 Release 仅提供 Linux amd64，暂不支持架构：$(uname -m)"
    ;;
esac

for command_name in curl sha256sum tar install systemctl getent useradd; do
  require_command "$command_name"
done

if [[ "$version" == "latest" ]]; then
  latest_url="$(curl --fail --silent --show-error --location \
    --output /dev/null --write-out '%{url_effective}' \
    "https://github.com/${REPOSITORY}/releases/latest")"
  version="${latest_url##*/}"
fi
[[ "$version" == v* ]] || fail "版本号必须以 v 开头，例如 v1.0.0"
[[ "$version" != *'/'* ]] || fail "版本号不能包含斜杠"

if [[ -n "$config_source" ]]; then
  [[ -f "$config_source" ]] || fail "配置文件不存在：$config_source"
elif [[ ! -f "$CONFIG_PATH" ]]; then
  fail "首次安装必须提供配置文件，例如：sudo ./install.sh ./.env"
fi

validate_config() {
  local file="$1"
  grep -Eq '^EWELINK_APP_ID=.+$' "$file" || fail "配置缺少 EWELINK_APP_ID"
  grep -Eq '^EWELINK_APP_SECRET=.+$' "$file" || fail "配置缺少 EWELINK_APP_SECRET"
  if grep -Eq '^EWELINK_(APP_ID|APP_SECRET)=<.*>$' "$file"; then
    fail "请先将配置文件中的占位值替换为真实值"
  fi
}

if [[ -n "$config_source" ]]; then
  validate_config "$config_source"
else
  validate_config "$CONFIG_PATH"
fi

temp_dir="$(mktemp -d)"
cleanup() {
  rm -rf -- "$temp_dir"
}
trap cleanup EXIT

asset="${PROGRAM}_${version}_linux_${architecture}.tar.gz"
release_base="https://github.com/${REPOSITORY}/releases/download/${version}"

printf '正在下载 %s...\n' "$asset"
curl --fail --silent --show-error --location \
  --output "$temp_dir/$asset" "$release_base/$asset"
curl --fail --silent --show-error --location \
  --output "$temp_dir/checksums.txt" "$release_base/checksums.txt"

expected_checksum="$(awk -v name="./$asset" '$2 == name {print $1}' \
  "$temp_dir/checksums.txt")"
[[ "$expected_checksum" =~ ^[0-9a-fA-F]{64}$ ]] || \
  fail "checksums.txt 中没有找到 $asset"
printf '%s  %s\n' "$expected_checksum" "$temp_dir/$asset" | sha256sum --check --status || \
  fail "下载文件的 SHA-256 校验失败"

tar -xzf "$temp_dir/$asset" -C "$temp_dir"
[[ -f "$temp_dir/$PROGRAM" ]] || fail "发布包中缺少 $PROGRAM"

script_dir=""
script_parent="$(dirname -- "$0")"
if [[ -d "$script_parent" ]]; then
  script_dir="$(cd -- "$script_parent" && pwd)"
fi
if [[ -n "$script_dir" && -f "$script_dir/$SERVICE_NAME" ]]; then
  service_source="$script_dir/$SERVICE_NAME"
else
  service_source="$temp_dir/$SERVICE_NAME"
  curl --fail --silent --show-error --location \
    --output "$service_source" \
    "https://raw.githubusercontent.com/${REPOSITORY}/${version}/deploy/${SERVICE_NAME}"
fi

if ! getent group ewelink >/dev/null 2>&1; then
  groupadd --system ewelink
fi
if ! id -u ewelink >/dev/null 2>&1; then
  useradd --system --gid ewelink --home-dir "$STATE_DIR" \
    --shell /usr/sbin/nologin ewelink
fi

install -d -o ewelink -g ewelink -m 0700 "$STATE_DIR"
if [[ -n "$config_source" ]]; then
  install -o root -g root -m 0600 "$config_source" "$CONFIG_PATH"
fi

install -o root -g root -m 0755 "$temp_dir/$PROGRAM" "${INSTALL_PATH}.new"
install -o root -g root -m 0644 "$service_source" "$SERVICE_PATH"

systemctl stop "$SERVICE_NAME" 2>/dev/null || true
mv -f -- "${INSTALL_PATH}.new" "$INSTALL_PATH"
systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME"

printf '\n%s %s 安装成功。\n' "$PROGRAM" "$version"
printf '服务状态：systemctl status %s\n' "$SERVICE_NAME"
printf '查看日志：journalctl -u %s -f\n' "$SERVICE_NAME"
printf '开始授权：http://127.0.0.1:33998/oauth/start\n'
