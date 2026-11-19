ARG SFOS_PLATFORM=linux/arm64

FROM --platform=$BUILDPLATFORM fedora:42 AS bootstrap
ARG SFOS_VERSION=5.1.0.11
ARG SFOS_ARCH=aarch64
ENV SFOS_VERSION=${SFOS_VERSION}
ENV SFOS_ARCH=${SFOS_ARCH}
COPY bootstrap.sh /bootstrap.sh
RUN bash /bootstrap.sh

FROM --platform=${SFOS_PLATFORM} scratch
ARG SFOS_VERSION=5.1.0.11
ARG SFOS_ARCH=aarch64
COPY --from=bootstrap /sfos /
ENV PATH=/usr/sbin:/usr/bin:/sbin:/bin
ENV HOME=/root
ENV LANG=C.UTF-8
ENV SFOS_VERSION=${SFOS_VERSION}
ENV SFOS_ARCH=${SFOS_ARCH}
COPY prepare.sh /usr/bin/sfos-prepare.sh
RUN set -eu; \
	ldconfig || true; \
	sh /usr/bin/sfos-prepare.sh

ARG SFOSBUILD_IMAGE_HASH=
LABEL sfosbuild.image-hash=${SFOSBUILD_IMAGE_HASH}
# @@sfosbuild-image-hooks@@
