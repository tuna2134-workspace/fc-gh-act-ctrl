# Rootfs image for the Firecracker microVM.
#
# Built by scripts/build-rootfs.sh, which provides the guest agent binary in the
# build context.
FROM ubuntu:24.04

ARG RUNNER_VERSION=2.338.1
ARG RUNNER_ARCH=x64

ENV DEBIAN_FRONTEND=noninteractive

# Prevent apt from starting services inside the build container.
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod 0755 /usr/sbin/policy-rc.d

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq tar gzip unzip xz-utils bzip2 \
      systemd systemd-sysv dbus udev \
      iproute2 iputils-ping netbase procps tzdata \
      libicu74 libssl3t64 zlib1g \
      sudo \
 && rm -rf /var/lib/apt/lists/*

# Actions runner. JIT configurations register the runner on first start, so no
# config.sh invocation is needed here.
RUN mkdir -p /opt/actions-runner \
 && curl -fsSL \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
      -o /tmp/runner.tar.gz \
 && tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner \
 && rm -f /tmp/runner.tar.gz \
 && /opt/actions-runner/bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/*

COPY fcghact-guest /usr/local/bin/fcghact-guest
COPY fcghact-guest.service /etc/systemd/system/fcghact-guest.service

RUN chmod 0755 /usr/local/bin/fcghact-guest \
 && mkdir -p /etc/systemd/system/multi-user.target.wants /etc/systemd/journald.conf.d \
 && ln -sf /etc/systemd/system/fcghact-guest.service /etc/systemd/system/multi-user.target.wants/fcghact-guest.service \
 && ln -sf /dev/null /etc/systemd/system/getty@tty1.service \
 && ln -sf /dev/null /etc/systemd/system/serial-getty@ttyS0.service \
 && printf '[Journal]\nStorage=volatile\nRuntimeMaxUse=64M\n' > /etc/systemd/journald.conf.d/99-fcghact.conf \
 && printf 'devpts /dev/pts devpts defaults 0 0\ntmpfs /dev/shm tmpfs defaults 0 0\n' > /etc/fstab \
 && truncate -s 0 /etc/machine-id \
 && : > /etc/hostname

WORKDIR /root
