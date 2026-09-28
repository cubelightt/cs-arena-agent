#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-only
# Copyright (C) 2026 cubelightt

# 构建 v2 桥二进制(单静态二进制,主机零运行时依赖)。
#
#   scripts/build.sh                 # 输出 ./cs
#   VERSION=v2.0 scripts/build.sh
#
# 说明:Go 工具链不在 PATH 时可用 GO=<路径> 指定;
set -euo pipefail

cd "$(dirname "$0")/.."

GO="${GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  if [ -x "$HOME/.local/go/bin/go" ]; then GO="$HOME/.local/go/bin/go"; else
    echo "找不到 go 工具链:装到 ~/.local/go 或用 GO=<路径> 指定" >&2
    exit 1
  fi
fi

VERSION="${VERSION:-v2.0}"
OUT="${OUT:-cs}"

echo "[build] $($GO version)"
CGO_ENABLED=0 "$GO" build -trimpath \
  -ldflags "-s -w -X main.version=${VERSION}" \
  -o "$OUT" ./cmd/cs
echo "[build] -> $OUT ($(stat -c%s "$OUT") bytes, version=${VERSION})"
