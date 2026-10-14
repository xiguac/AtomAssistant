#!/usr/bin/env bash
# 在服务器上安装代码沙箱：一个独立的 Python 环境，装好处理文件常用的库，并让服务用上它。
#
# 由 deploy/deploy.sh sandbox 调用，以 root 身份在服务器上运行。
# 环境装在 /opt/atomhelper-sandbox：root 所有，服务账号只读——沙箱里的代码改不了它。
# 隔离靠 bubblewrap，跑起来的代码断网、只读看到工作区，见 internal/sandbox。
set -euo pipefail

DIR=/opt/atomhelper-sandbox
CONFIG=/var/lib/atomhelper/config/config.yaml
# 服务器在国内，PyPI 官方源慢且不稳，用腾讯云的镜像。
INDEX=${SANDBOX_PIP_INDEX:-https://mirrors.tencentyun.com/pypi/simple}

echo "==> 安装 bubblewrap 与 Python 虚拟环境支持"
apt-get update -qq
apt-get install -y -qq bubblewrap util-linux python3-venv fonts-noto-cjk >/dev/null

echo "==> 创建独立的 Python 环境 $DIR"
if [ ! -x "$DIR/bin/python3" ]; then
  python3 -m venv "$DIR"
fi
"$DIR/bin/python3" -m pip install -q --upgrade pip -i "$INDEX"
"$DIR/bin/python3" -m pip install -q -i "$INDEX" \
  openpyxl python-docx python-pptx pypdf pandas numpy matplotlib pillow
chown -R root:root "$DIR"
chmod -R go-w "$DIR"

echo "==> 以服务账号、按服务的限制试跑一次"
T=$(mktemp -d)
mkdir -p "$T/work" "$T/out"
echo "hello" > "$T/work/a.txt"
cat > "$T/main.py" <<'PY'
import socket, os
import openpyxl, docx, pptx, pypdf, pandas, numpy, matplotlib, PIL
print("libs ok")
print(open("/work/a.txt").read().strip())
try:
    socket.create_connection(("1.1.1.1", 80), timeout=3)
    print("NETWORK REACHABLE")
except OSError:
    print("no network")
print("etc atomhelper visible" if os.path.exists("/etc/atomhelper") else "etc atomhelper hidden")
open("/out/result.txt", "w").write("done")
PY
chown -R atomhelper:atomhelper "$T"
systemd-run --wait --pipe --quiet -p User=atomhelper -p NoNewPrivileges=true -p ProtectSystem=strict \
  -p ProtectHome=true -p PrivateTmp=true -p PrivateDevices=true -p RestrictSUIDSGID=true -p ReadWritePaths="$T" \
  bwrap --die-with-parent --new-session --unshare-all \
    --ro-bind /usr /usr --symlink usr/lib /lib --symlink usr/lib64 /lib64 --symlink usr/bin /bin --symlink usr/sbin /sbin \
    --ro-bind-try /etc/ld.so.cache /etc/ld.so.cache --ro-bind-try /etc/fonts /etc/fonts \
    --ro-bind "$DIR" "$DIR" --proc /proc --dev /dev --tmpfs /tmp \
    --ro-bind "$T/work" /work --bind "$T/out" /out --ro-bind "$T/main.py" /sandbox/main.py --chdir /out \
    --clearenv --setenv HOME /tmp --setenv MPLCONFIGDIR /tmp --setenv LANG C.UTF-8 -- \
    prlimit --as=1073741824 --cpu=60 -- "$DIR/bin/python3" -I /sandbox/main.py
test "$(cat "$T/out/result.txt")" = "done"
rm -rf "$T"

if [ -f "$CONFIG" ] && ! grep -q '^sandbox:' "$CONFIG"; then
  echo "==> 在配置里启用沙箱"
  printf '\n# ---- 代码沙箱（deploy.sh sandbox 写入） ----\nsandbox: {}\n' >> "$CONFIG"
fi
echo "==> 沙箱已就绪，重启服务后生效"
