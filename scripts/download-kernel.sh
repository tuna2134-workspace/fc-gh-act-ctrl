#!/usr/bin/env bash
# Download a Firecracker-compatible guest kernel from the Firecracker CI bucket.
set -euo pipefail

KERNEL_VERSION="${KERNEL_VERSION:-6.1.128}"
CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.12}"
OUT="${OUT:-/var/lib/fcghact/vmlinux}"

case "$(uname -m)" in
  x86_64) ARCH=x86_64 ;;
  aarch64 | arm64) ARCH=aarch64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ "${EUID}" -ne 0 ]]; then
  echo "download-kernel.sh must run as root (writes to /var/lib/fcghact)" >&2
  exit 1
fi

URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}"
echo "==> downloading ${URL}"
mkdir -p "$(dirname "${OUT}")"
curl -fL --retry 3 -o "${OUT}.tmp" "${URL}"
mv "${OUT}.tmp" "${OUT}"
chmod 0644 "${OUT}"

echo "kernel written to ${OUT}"
