# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# KingresDB container image: a node that starts uninitialized and shows the
# first-run wizard.  Two stages:
#   build    runs kingres/packaging/build/build.sh (the release recipe) in
#            an Ubuntu 24.04 build environment;
#   runtime  a minimal Ubuntu 24.04 with only the run-time libraries, a
#            non-root user and the installation tree.
#
# Build (context = repository root or a clean snapshot, see README.md):
#   docker build -f kingres/packaging/docker/Dockerfile -t kingres:0.1.0 .
#
# The base image is pinned by digest for reproducibility.  To move to a newer
# Ubuntu 24.04 build, pull "ubuntu:24.04", read its digest with
#   docker image inspect ubuntu:24.04 --format '{{index .RepoDigests 0}}'
# and change the default below in a reviewed commit (apt packages are not
# pinned: they are the security updates of that Ubuntu release).
ARG UBUNTU_IMAGE=ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5

# ------------------------------------------------------------------ build
FROM ${UBUNTU_IMAGE} AS build
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential meson ninja-build pkg-config bison flex perl python3 \
        libreadline-dev zlib1g-dev libicu-dev libssl-dev liblz4-dev libzstd-dev \
        libsystemd-dev libxml2-dev liburing-dev libnuma-dev uuid-dev \
        dpkg-dev file git curl xz-utils ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# The pinned Go and Node.js (checksum-verified); a separate layer so that a
# source change does not download them again.
COPY kingres/packaging/build/versions.env kingres/packaging/build/toolchain.sh /opt/kingres-build/
RUN bash /opt/kingres-build/toolchain.sh /opt/kingres-toolchain
ENV PATH=/opt/kingres-toolchain/go/bin:/opt/kingres-toolchain/node/bin:${PATH}

COPY . /src
ARG SOURCE_DATE_EPOCH
RUN KINGRES_BUILD_AS_ROOT=1 bash /src/kingres/packaging/build/build.sh \
        --src /src --work /work --jobs "$(nproc)"

# ---------------------------------------------------------------- runtime
FROM ${UBUNTU_IMAGE} AS runtime
ENV DEBIAN_FRONTEND=noninteractive

# Run-time packages: exactly what dpkg-shlibdeps found for the installed
# programs (version constraints and alternatives removed), plus tzdata (time
# zone rules), tini (reaps the instances kingresd starts) and CA certificates.
COPY --from=build /work/info/shlibs.depends /tmp/shlibs.depends
RUN apt-get update \
 && packages="$(sed -e 's/([^)]*)//g' -e 's/|[^,]*//g' -e 's/,/ /g' /tmp/shlibs.depends)" \
 && apt-get install -y --no-install-recommends ${packages} tzdata tini ca-certificates \
 && rm -rf /var/lib/apt/lists/* /tmp/shlibs.depends

# A fixed uid/gid so that bind-mounted volumes can be prepared on the host.
RUN groupadd --gid 10001 kingres \
 && useradd --uid 10001 --gid kingres --home-dir /var/lib/kingres \
        --no-create-home --shell /usr/sbin/nologin kingres \
 && install -d -o kingres -g kingres -m 0750 /var/lib/kingres

COPY --from=build /work/stage/opt/kingres /opt/kingres
COPY kingres/packaging/docker/entrypoint.sh /usr/local/bin/kingres-entrypoint
COPY kingres/packaging/docker/healthcheck.sh /usr/local/bin/kingres-healthcheck
RUN chmod 0755 /usr/local/bin/kingres-entrypoint /usr/local/bin/kingres-healthcheck

# No secrets here: the setup token is created at first start in the volume,
# the administrator is created in the wizard.
#   KINGRES_ALLOW_REMOTE_SETUP  the panel listens on 0.0.0.0 (a published port
#                               must reach it), so setup calls from other
#                               addresses are allowed; the setup token is still
#                               required.  Publish the port to 127.0.0.1 unless
#                               you mean otherwise.
#   KINGRES_PG_LISTEN           the instance accepts TCP on all interfaces of
#                               the container; pg_hba.conf (managed by kingresd)
#                               decides who may log in.
# The instance's data and Unix sockets live in the volume
# (/var/lib/kingres/instances/1/pgdata, /var/lib/kingres/run/1).
ENV KINGRES_LISTEN=0.0.0.0:8091 \
    KINGRES_ALLOW_REMOTE_SETUP=true \
    KINGRES_PG_LISTEN=0.0.0.0 \
    KINGRES_PG_PORT=5432 \
    KINGRES_DATA_DIR=/var/lib/kingres \
    KINGRES_LOG_FORMAT=json \
    KINGRES_PG_BIN_DIR=/opt/kingres/bin \
    PATH=/opt/kingres/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ARG VERSION=0.1.0
ARG REVISION=unknown
LABEL org.opencontainers.image.title="KingresDB" \
      org.opencontainers.image.description="KingresDB node with the management panel" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.vendor="RooyeKhat Media Co" \
      org.opencontainers.image.licenses="PostgreSQL"

USER kingres:kingres
WORKDIR /var/lib/kingres
VOLUME ["/var/lib/kingres"]
EXPOSE 8091 5432
# Stopping shuts the database down first (see entrypoint.sh): give it time,
# e.g. docker run --stop-timeout 120 (compose.yaml does).
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/kingres-healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/kingres-entrypoint"]
CMD []
