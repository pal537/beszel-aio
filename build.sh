#!/bin/bash
# 构建 beszel-aio 的 fpk:
#   1) 从 Beszel 官方 Release 下载 v0.21.0 二进制并校验 SHA-256
#   2) 用 Go 编译包装网关 beszel-gateway
#   3) 用 fnpack 打包(需 fnpack 在 PATH 中, 或用 FNPACK=/path/to/fnpack 指定)
set -e
cd "$(dirname "$0")"
VER=0.21.0
BASE="https://github.com/henrygd/beszel/releases/download/v${VER}"
mkdir -p .dl package/app/bin

for f in beszel_linux_amd64.tar.gz beszel-agent_linux_amd64.tar.gz "beszel_${VER}_checksums.txt"; do
    if [ ! -f ".dl/$f" ]; then
        echo "downloading $f"
        curl -fL -o ".dl/$f" "$BASE/$f"
    fi
done

echo "verifying checksums"
(cd .dl && grep -E " (beszel|beszel-agent)_linux_amd64\.tar\.gz$" "beszel_${VER}_checksums.txt" | sha256sum -c -)

tar -xzf .dl/beszel_linux_amd64.tar.gz -C .dl beszel LICENSE
tar -xzf .dl/beszel-agent_linux_amd64.tar.gz -C .dl beszel-agent
cp .dl/beszel .dl/beszel-agent package/app/bin/

echo "building gateway"
(cd gateway && go build -trimpath -o ../package/app/bin/beszel-gateway .)

echo "packing fpk"
(cd package && "${FNPACK:-fnpack}" build)
echo "done: package/beszel-aio.fpk"
