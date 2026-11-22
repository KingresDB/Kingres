#!/usr/bin/env bash
#
# kingres/dev/setup-ubuntu.sh
#
# One-time preparation of an Ubuntu 24.04 machine (or WSL2 distribution) for
# building and testing KingresDB.  Run it as root.
#
# It does two things:
#   1. installs the toolchain and libraries the PostgreSQL meson build needs;
#   2. creates an unprivileged user, because PostgreSQL refuses to run as
#      root and the test suites start real servers.
#
# Usage (as root):
#   kingres/dev/setup-ubuntu.sh [username]      # default username: kingres
#
# The script is idempotent: running it again changes nothing.

set -euo pipefail

dev_user="${1:-kingres}"

if [ "$(id -u)" -ne 0 ]; then
    echo "setup-ubuntu.sh: error: must be run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

# Toolchain and build system, parser generators, and the libraries behind the
# features we build with (readline, zlib, ICU collations, TLS, LZ4 and Zstandard
# compression).  libipc-run-perl is required by the TAP test suites.
apt-get update -qq
apt-get install -y -qq \
    build-essential meson ninja-build pkg-config \
    bison flex perl python3 \
    libreadline-dev zlib1g-dev libicu-dev libssl-dev liblz4-dev libzstd-dev \
    libipc-run-perl \
    rsync git ccache

if ! id -u "${dev_user}" >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash "${dev_user}"
fi

# Under WSL2, make the unprivileged user the default one for new sessions.
if grep -qi microsoft /proc/version 2>/dev/null; then
    if ! grep -q '^\[user\]' /etc/wsl.conf 2>/dev/null; then
        printf '\n[user]\ndefault=%s\n' "${dev_user}" >>/etc/wsl.conf
    fi
fi

echo "setup complete; build as user '${dev_user}' with kingres/dev/build.sh"
