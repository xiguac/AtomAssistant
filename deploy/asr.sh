#!/usr/bin/env bash
# 在服务器上安装语音识别服务：聊天里发来的语音消息由它转成文字再交给助手。
#
# 由 deploy/deploy.sh asr 调用，以 root 身份在服务器上运行。同目录里要有：
#   asr.tar.gz               识别程序（rt/）、模型（models/）、自检音频（samples/）和 selfcheck.py
#   atomassistant-asr.service   服务的 systemd 配置
# 程序和模型装在 /opt/atomassistant-asr，root 所有，服务只读。服务只听 127.0.0.1:8765。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DIR=/opt/atomassistant-asr
CONFIG=/var/lib/atomassistant/config/config.yaml
PORT=8765
# 服务器在国内，PyPI 官方源慢且不稳，用腾讯云的镜像。
INDEX=${ASR_PIP_INDEX:-https://mirrors.tencentyun.com/pypi/simple}

# 安装时的自检要现场解码一遍模型，峰值约 2.4 GB。装好之后服务用解码好的缓存（见 rt/wcache.py）：
# 必须常驻的约 0.6 GB；编码器那 1.2 GB 是映射的缓存文件，内存紧张时系统可以回收。
avail_mb=$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)
echo "==> 服务器当前可用内存 ${avail_mb} MB（安装自检峰值约 2400 MB，之后常驻约 600 MB）"
if [ "$avail_mb" -lt 2600 ]; then
  echo "!! 可用内存不足 2.6 GB，安装时的自检可能被系统杀掉，或者挤占助手本身的内存" >&2
fi

echo "==> 安装 Python 虚拟环境支持"
apt-get update -qq
apt-get install -y -qq python3-venv >/dev/null

echo "==> 创建独立的 Python 环境 $DIR/venv"
install -d -o root -g root -m 755 "$DIR"
if [ ! -x "$DIR/venv/bin/python3" ]; then
  python3 -m venv "$DIR/venv"
fi
"$DIR/venv/bin/python3" -m pip install -q --upgrade pip -i "$INDEX"
"$DIR/venv/bin/python3" -m pip install -q -i "$INDEX" numpy

echo "==> 放好识别程序和模型"
rm -rf "$DIR/app.new"
mkdir -p "$DIR/app.new"
tar -xzf "$HERE/asr.tar.gz" -C "$DIR/app.new"
chown -R root:root "$DIR"
chmod -R go-w "$DIR"

THREADS=$(nproc)
[ "$THREADS" -gt 8 ] && THREADS=8

# 自检要再起一份识别程序：先停掉在跑的那份，4G 内存的机器放不下两份（同时跑会把机器拖进 swap 卡死）。
# 停的这一会儿发来的语音会被告知没转成；自检不过就把旧的重新起来。
systemctl stop atomassistant-asr 2>/dev/null || true
echo "==> 自检：跑自带的 12 条音频（载入加预热要十几秒）"
if ! (cd "$DIR/app.new" && PYTHONPATH="$DIR/app.new" PYTHONIOENCODING=utf-8 ASR_BLAS_THREADS="$THREADS" \
  ASR_MODELS="$DIR/app.new/models/unpacked_v6" "$DIR/venv/bin/python3" selfcheck.py); then
  echo "!! 自检没通过，新版本不装，旧的照常运行" >&2
  [ -d "$DIR/app" ] && systemctl start atomassistant-asr || true
  exit 1
fi

# 解码好的权重存成缓存，服务启动时直接映射进来，不用再解码。
# 缓存必须和现场解码逐位一致：两边各跑一遍自检音频，比对每个权重、编码器输出、识别出的每个 token。
echo "==> 生成解码后权重的缓存，并与现场解码逐位比对"
app_py() {
  (cd "$DIR/app.new" && PYTHONPATH="$DIR/app.new" PYTHONIOENCODING=utf-8 ASR_BLAS_THREADS="$THREADS"     ASR_MODELS="$DIR/app.new/models/unpacked_v6" ASR_WEIGHT_CACHE="$DIR/app.new/wcache" "$DIR/venv/bin/python3" "$@")
}
CHK=$(mktemp -d)
if ! app_py -m rt.wcache build ||
  ! ASR_WEIGHT_CACHE_OFF=1 app_py -m rt.wcache digest "$CHK/fresh.json" ||
  ! app_py -m rt.wcache digest "$CHK/cached.json" ||
  ! app_py -m rt.wcache compare "$CHK/fresh.json" "$CHK/cached.json"; then
  rm -rf "$CHK"
  echo "!! 缓存与现场解码不一致，新版本不装，旧的照常运行" >&2
  [ -d "$DIR/app" ] && systemctl start atomassistant-asr || true
  exit 1
fi
rm -rf "$CHK"
chmod -R go-w,go+rX "$DIR/app.new/wcache"

rm -rf "$DIR/app.old"
[ -d "$DIR/app" ] && mv "$DIR/app" "$DIR/app.old"
mv "$DIR/app.new" "$DIR/app"

echo "==> 安装并启动服务"
sed "s/@THREADS@/$THREADS/" "$HERE/atomassistant-asr.service" > /etc/systemd/system/atomassistant-asr.service
systemctl daemon-reload
systemctl enable -q atomassistant-asr
systemctl restart atomassistant-asr
ready=""
for _ in $(seq 1 60); do
  if curl -fsS -m 3 "http://127.0.0.1:$PORT/api/status" 2>/dev/null | grep -q '"ready"'; then
    ready=1
    break
  fi
  sleep 1
done
if [ -z "$ready" ]; then
  echo "!! 识别服务一分钟内没有就绪。最近的日志：" >&2
  journalctl -u atomassistant-asr -n 30 --no-pager >&2 || true
  exit 1
fi
rm -rf "$DIR/app.old"

if [ -f "$CONFIG" ] && ! grep -q '^asr:' "$CONFIG"; then
  echo "==> 在配置里启用语音识别"
  printf '\n# ---- 语音识别（deploy.sh asr 写入） ----\nasr:\n  url: http://127.0.0.1:%s\n' "$PORT" >> "$CONFIG"
fi
echo "==> 语音识别已就绪，重启助手后生效"
