#!/usr/bin/env bash
# Install the Firecracker VMM (and jailer) from GitHub releases.
set -euo pipefail

VERSION="${FIRECRACKER_VERSION:-1.17.0}"
DEST="${DEST:-/usr/local/bin}"

case "$(uname -m)" in
  x86_64) ARCH=x86_64 ;;
  aarch64 | arm64) ARCH=aarch64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ "${EUID}" -ne 0 ]]; then
  echo "install-firecracker.sh must run as root" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

URL="https://github.com/firecracker-microvm/firecracker/releases/download/v${VERSION}/firecracker-v${VERSION}-${ARCH}.tgz"
echo "==> downloading ${URL}"
curl -fsSL --retry 3 -o "${WORK}/firecracker.tgz" "${URL}"
tar -xzf "${WORK}/firecracker.tgz" -C "${WORK}"

SRC="${WORK}/release-v${VERSION}-${ARCH}"
install -m 0755 "${SRC}/firecracker-v${VERSION}-${ARCH}" "${DEST}/firecracker"
install -m 0755 "${SRC}/jailer-v${VERSION}-${ARCH}" "${DEST}/jailer"

"${DEST}/firecracker" --version
