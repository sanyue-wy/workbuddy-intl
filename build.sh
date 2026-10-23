#!/usr/bin/env bash
# WorkBuddy 国际版多平台跨平台编译脚本 (Bash)
set -euo pipefail

VERSION="${1:-1.0.0}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

DIST_DIR="${SCRIPT_DIR}/dist"
mkdir -p "${DIST_DIR}"

export CGO_ENABLED=0

TARGETS=(
  "windows/amd64/.exe"
  "windows/arm64/.exe"
  "linux/amd64/"
  "linux/arm64/"
  "darwin/amd64/"
  "darwin/arm64/"
)

echo "=========================================================="
echo " WorkBuddy 国际版 (workbuddy-intl) 多平台二进制编译开始 v${VERSION}"
echo "=========================================================="

for target in "${TARGETS[@]}"; do
  IFS='/' read -r OS ARCH EXT <<< "${target}"
  OUT="${DIST_DIR}/workbuddy-intl-${OS}-${ARCH}${EXT}"
  echo "[BUILD] 正在构建 ${OS}/${ARCH} -> ${OUT} ..."
  GOOS="${OS}" GOARCH="${ARCH}" go build -ldflags="-s -w -X main.version=${VERSION}" -trimpath -o "${OUT}" main.go
done

echo ""
echo "[CHECKSUM] 计算 SHA256 校验和并生成 Markdown 校验表..."
CHECKSUM_FILE="${DIST_DIR}/checksums.txt"
CHECKSUM_MD="${DIST_DIR}/checksums.md"
rm -f "${CHECKSUM_FILE}" "${CHECKSUM_MD}"

cd "${DIST_DIR}"
for f in workbuddy-intl-*; do
  if [ -f "${f}" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum "${f}" >> checksums.txt
    elif command -v shasum >/dev/null 2>&1; then
      shasum -a 256 "${f}" >> checksums.txt
    fi
  fi
done

# 生成 Markdown 表格
{
  echo "<!-- CHECKSUM_TABLE_START -->"
  echo "<!-- 本校验和表由 build.sh 自动生成，请勿手动编辑 -->"
  echo "| 目标平台 | 二进制文件名 | SHA-256 校验和 |"
  echo "| :--- | :--- | :--- |"
  while read -r hash file; do
    [ -z "${hash}" ] && continue
    file="${file#\*}"
    file="${file# }"
    platform="${file#workbuddy-intl-}"
    platform="${platform%.exe}"
    echo "| \`${platform}\` | \`${file}\` | \`${hash}\` |"
  done < "${CHECKSUM_FILE}"
  echo "<!-- CHECKSUM_TABLE_END -->"
} > "${CHECKSUM_MD}"

# 若根目录下存在 README.md 且包含占位标记，则自动回写替换
README_FILE="${SCRIPT_DIR}/README.md"
if [ -f "${README_FILE}" ] && grep -q "CHECKSUM_TABLE_START" "${README_FILE}"; then
  echo "[README] 正在同步更新 README.md 中的校验和表..."
  TABLE_CONTENT=$(cat "${CHECKSUM_MD}")
  python3 -c "
import sys
content = open('${README_FILE}', 'r', encoding='utf-8').read()
new_table = open('${CHECKSUM_MD}', 'r', encoding='utf-8').read()
start = content.find('<!-- CHECKSUM_TABLE_START -->')
end = content.find('<!-- CHECKSUM_TABLE_END -->')
if start != -1 and end != -1:
    end += len('<!-- CHECKSUM_TABLE_END -->')
    updated = content[:start] + new_table + content[end:]
    open('${README_FILE}', 'w', encoding='utf-8').write(updated)
" 2>/dev/null || true
fi

echo "=========================================================="
echo " 构建完成！全部 6 大平台纯净二进制均已存入 dist/ 目录"
echo " 校验和已输出至 dist/checksums.txt 与 dist/checksums.md"
echo "=========================================================="
