#!/usr/bin/env bash
# 在服务器上安装 Skill 扫描器 SkillSpector，并让服务用上它。
# 由 deploy.sh scanner 上传并以 root 身份运行；可以重复运行。
#
# 装在 /opt/skillspector 的独立环境里：root 所有，服务账号只读。
# 默认只做静态扫描（--no-llm）；要它连模型做语义分析，自己在 /etc/atomhelper/env 里改参数并提供密钥。
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "要用 root 运行（sudo bash scanner.sh）" >&2
  exit 1
fi

DIR=/opt/skillspector
ENV_FILE=/etc/atomhelper/env
BIN=$DIR/bin/skillspector

echo "==> 安装 Python 的虚拟环境支持、git 与编译工具（已安装则跳过）"
# 它依赖的 yara-python 在较新的 Python 上没有现成的包，要在本机编译，所以需要头文件和编译器。
if ! python3 -c 'import ensurepip' >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1 ||
  ! command -v gcc >/dev/null 2>&1 || [ ! -f "$(python3 -c 'import sysconfig; print(sysconfig.get_paths()["include"])')/Python.h" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq python3-venv python3-dev build-essential libssl-dev git >/dev/null
fi

echo "==> 安装 SkillSpector 到 $DIR"
[ -x "$DIR/bin/python" ] || python3 -m venv "$DIR"
# 旁边放了源码包就用它：从国内的服务器直接连 GitHub 经常连不上，deploy.sh 会在本机下好一并传上来。
SRC=$(cd "$(dirname "$0")" && pwd)/skillspector.tar.gz
if [ -f "$SRC" ]; then
  "$DIR/bin/python" -m pip install -q --upgrade "$SRC"
else
  "$DIR/bin/python" -m pip install -q --upgrade "git+https://github.com/NVIDIA/skillspector.git"
fi
# 服务账号写不了这个目录，字节码现在就编好。
"$DIR/bin/python" -m compileall -q "$DIR/lib" >/dev/null || true
chown -R root:root "$DIR"
chmod -R go+rX "$DIR"
"$BIN" --help >/dev/null

echo "==> 写入服务的环境变量（已有则保留）"
[ -f "$ENV_FILE" ] || { echo "没有 $ENV_FILE，先运行 deploy.sh setup" >&2; exit 1; }
if ! grep -q '^ATOM_SKILL_SCANNER=' "$ENV_FILE"; then
  cp -p "$ENV_FILE" "$ENV_FILE.bak-$(date +%Y%m%d-%H%M%S)"
  {
    echo "# Skill 与 MCP server 安装前的扫描器。没有它，助手什么都装不上。"
    echo "ATOM_SKILL_SCANNER=$BIN"
    echo "ATOM_SKILL_SCANNER_ARGS=--no-llm"
  } >> "$ENV_FILE"
fi

echo "==> 以服务账号、在服务的权限限制下试扫一次"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir "$T/sample"
printf -- '---\nname: sample\ndescription: 试扫用\n---\n\n# 示例\n\n先列出要点，再整理成文。\n' > "$T/sample/SKILL.md"
chmod -R a+rX "$T"
# 与 atomhelper.service 里的限制一致：只读的系统目录、看不到 /home。
systemd-run --quiet --wait --pipe --collect \
  -p User=atomhelper -p Group=atomhelper \
  -p ProtectSystem=strict -p ProtectHome=true -p NoNewPrivileges=true \
  -p ReadWritePaths=/var/lib/atomhelper -p WorkingDirectory=/var/lib/atomhelper \
  -p BindReadOnlyPaths="$T" \
  env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/var/lib/atomhelper \
  "$BIN" scan "$T/sample" --no-llm >/dev/null
echo "    试扫通过"
echo "==> 扫描器已就绪。重启服务后生效：systemctl restart atomhelper"
