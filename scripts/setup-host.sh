#!/usr/bin/env bash
# Create the microVM bridge and enable NAT for guest egress.
set -euo pipefail

BRIDGE="${BRIDGE:-fcghact0}"
CIDR="${CIDR:-172.31.0.0/16}"
GATEWAY="${GATEWAY:-172.31.0.1}"
PREFIX="${CIDR#*/}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "setup-host.sh must run as root" >&2
  exit 1
fi

if [[ ! -e /dev/kvm ]]; then
  echo "warning: /dev/kvm is missing; load kvm_intel/kvm_amd and enable virtualization" >&2
fi

echo "==> bridge ${BRIDGE} (${GATEWAY}/${PREFIX})"
if ! ip link show "${BRIDGE}" >/dev/null 2>&1; then
  ip link add name "${BRIDGE}" type bridge
fi
if ! ip -4 addr show dev "${BRIDGE}" | grep -q "inet ${GATEWAY}/"; then
  ip addr add "${GATEWAY}/${PREFIX}" dev "${BRIDGE}"
fi
ip link set "${BRIDGE}" up

echo "==> enable IPv4 forwarding"
install -d /etc/sysctl.d
printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/99-fcghact.conf
sysctl -q -w net.ipv4.ip_forward=1

echo "==> install NAT rules"
if ! iptables -t nat -C POSTROUTING -s "${CIDR}" ! -o "${BRIDGE}" -j MASQUERADE 2>/dev/null; then
  iptables -t nat -A POSTROUTING -s "${CIDR}" ! -o "${BRIDGE}" -j MASQUERADE
fi
if ! iptables -C FORWARD -i "${BRIDGE}" -j ACCEPT 2>/dev/null; then
  iptables -A FORWARD -i "${BRIDGE}" -j ACCEPT
fi
if ! iptables -C FORWARD -o "${BRIDGE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  iptables -A FORWARD -o "${BRIDGE}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
fi

cat <<EOF

host network ready.

To persist iptables rules install iptables-persistent (Debian/Ubuntu) or a
systemd unit that re-runs this script at boot.

EOF
