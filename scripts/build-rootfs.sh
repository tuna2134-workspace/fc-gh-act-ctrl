#!/usr/bin/env bash
# Build a bootable ext4 rootfs containing the Actions runner and guest agent.
#
# Requirements: root, docker (or podman), e2fsprogs >= 1.43 (mkfs.ext4 -d).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${OUT_DIR:-${ROOT_DIR}/.build}"
ROOTFS_IMAGE="${ROOTFS_IMAGE:-/var/lib/fcghact/rootfs.ext4}"
ROOTFS_SIZE="${ROOTFS_SIZE:-4G}"
RUNNER_VERSION="${RUNNER_VERSION:-2.338.1}"
CONTAINER_RUNTIME="${CONTAINER_RUNTIME:-}"
case "$(uname -m)" in
  aarch64 | arm64) GOARCH=arm64; RUNNER_ARCH=arm64 ;;
  *) GOARCH=amd64; RUNNER_ARCH=x64 ;;
esac

if [[ "${EUID}" -ne 0 ]]; then
  echo "build-rootfs.sh must run as root (it preserves file ownership from the container)" >&2
  exit 1
fi

case "${OUT_DIR}" in
  "" | "/")
    echo "refusing to use OUT_DIR=${OUT_DIR}" >&2
    exit 1
    ;;
esac
case "${ROOTFS_IMAGE}" in
  "" | "/")
    echo "refusing to write ROOTFS_IMAGE=${ROOTFS_IMAGE}" >&2
    exit 1
    ;;
esac

if [[ -z "${CONTAINER_RUNTIME}" ]]; then
  if command -v docker >/dev/null 2>&1; then
    CONTAINER_RUNTIME=docker
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_RUNTIME=podman
  else
    echo "docker or podman is required" >&2
    exit 1
  fi
fi
command -v mkfs.ext4 >/dev/null || { echo "mkfs.ext4 is required" >&2; exit 1; }

CONTEXT="${OUT_DIR}/rootfs-context"
ROOTFS_DIR="${OUT_DIR}/rootfs-root"
rm -rf "${CONTEXT}" "${ROOTFS_DIR}"
mkdir -p "${CONTEXT}" "${ROOTFS_DIR}"

echo "==> preparing guest agent"
if [[ -n "${GUEST_AGENT_BINARY:-}" ]]; then
  install -m 0755 "${GUEST_AGENT_BINARY}" "${CONTEXT}/fcghact-guest"
else
  CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" \
    go build -trimpath -ldflags "-s -w" -o "${CONTEXT}/fcghact-guest" "${ROOT_DIR}/guest/agent"
fi

cp "${ROOT_DIR}/images/rootfs/Dockerfile" "${CONTEXT}/Dockerfile"
cp "${ROOT_DIR}/images/rootfs/fcghact-guest.service" "${CONTEXT}/fcghact-guest.service"

echo "==> building container image (runner ${RUNNER_VERSION})"
"${CONTAINER_RUNTIME}" build \
  --build-arg "RUNNER_VERSION=${RUNNER_VERSION}" \
  --build-arg "RUNNER_ARCH=${RUNNER_ARCH}" \
  -t "fcghact-rootfs:${RUNNER_VERSION}" \
  "${CONTEXT}"

echo "==> exporting container filesystem"
cid="$("${CONTAINER_RUNTIME}" create "fcghact-rootfs:${RUNNER_VERSION}")"
cleanup() {
  "${CONTAINER_RUNTIME}" rm -f "${cid}" >/dev/null 2>&1 || true
  rm -rf "${CONTEXT}" "${ROOTFS_DIR}"
}
trap cleanup EXIT
"${CONTAINER_RUNTIME}" export "${cid}" | tar -x -C "${ROOTFS_DIR}" --numeric-owner -p

# The container runtime mounts /dev, /proc and /sys as tmpfs, so recreate the
# mount points systemd expects.
mkdir -p "${ROOTFS_DIR}"/{dev,proc,sys,run,tmp,etc}
chmod 1777 "${ROOTFS_DIR}/tmp"
: > "${ROOTFS_DIR}/etc/hostname"
: > "${ROOTFS_DIR}/etc/resolv.conf"
cat > "${ROOTFS_DIR}/etc/hosts" <<'EOF'
127.0.0.1 localhost
::1 localhost ip6-localhost ip6-loopback
EOF

echo "==> writing ${ROOTFS_IMAGE} (${ROOTFS_SIZE})"
mkdir -p "$(dirname "${ROOTFS_IMAGE}")"
rm -f "${ROOTFS_IMAGE}"
mkfs.ext4 -q -F -L fcghact-rootfs -d "${ROOTFS_DIR}" "${ROOTFS_IMAGE}" "${ROOTFS_SIZE}"

echo
echo "rootfs written to ${ROOTFS_IMAGE}"

if [[ -n "${ROOTFS_PUSH_REF:-}" ]]; then
  FCGHACT_BIN="${FCGHACT_BIN:-${ROOT_DIR}/bin/fcghact}"
  if [[ ! -x "${FCGHACT_BIN}" ]]; then
    echo "ROOTFS_PUSH_REF is set but ${FCGHACT_BIN} does not exist; run 'make build' first" >&2
    exit 1
  fi
  push_args=(--file "${ROOTFS_IMAGE}" --ref "${ROOTFS_PUSH_REF}")
  [[ -n "${ROOTFS_PUSH_TOKEN_ENV:-}" ]] && push_args+=(--token-env "${ROOTFS_PUSH_TOKEN_ENV}")
  [[ -n "${ROOTFS_PUSH_TOKEN:-}" ]] && push_args+=(--token "${ROOTFS_PUSH_TOKEN}")
  [[ -n "${ROOTFS_PUSH_INSECURE:-}" ]] && push_args+=(--insecure)
  echo "==> pushing rootfs to ${ROOTFS_PUSH_REF}"
  "${FCGHACT_BIN}" rootfs push "${push_args[@]}"
fi

echo "verify with: sudo bin/fcghact doctor --config /etc/fcghact/fcghact.yaml"
