ARG SFOS_PLATFORM=linux/arm64

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS tools
RUN apt-get update \
	&& apt-get install -y --no-install-recommends p7zip-full \
	&& rm -rf /var/lib/apt/lists/*

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS dl
ARG SFOS_VERSION=5.1.0.11
ARG SFOS_ARCH=aarch64
ADD https://releases.sailfishos.org/sdk/targets/Sailfish_OS-${SFOS_VERSION}-Sailfish_SDK_Target-${SFOS_ARCH}.tar.7z \
	/dl/target.tar.7z

FROM tools AS unpack
COPY unpack.sh /unpack.sh
COPY --from=dl /dl/target.tar.7z /dl/target.tar.7z
ARG TARGET_7Z_MD5=
ENV TARGET_7Z_MD5=${TARGET_7Z_MD5}
RUN sh /unpack.sh

FROM --platform=${SFOS_PLATFORM} scratch
ARG SFOS_VERSION=5.1.0.11
ARG SFOS_ARCH=aarch64
COPY --from=unpack /sfos /
ENV PATH=/usr/sbin:/usr/bin:/sbin:/bin
ENV HOME=/root
ENV LANG=C.UTF-8
ENV SFOS_VERSION=${SFOS_VERSION}
ENV SFOS_ARCH=${SFOS_ARCH}
COPY prepare.sh /usr/bin/sfos-prepare.sh
RUN rpm --rebuilddb || true \
	&& sh /usr/bin/sfos-prepare.sh

COPY image-scripts/ /tmp/sfosbuild-image/
ARG SFOSBUILD_IMAGE_HASH=
LABEL sfosbuild.image-hash=${SFOSBUILD_IMAGE_HASH}
RUN for s in /tmp/sfosbuild-image/*.sh; do \
	if [ -f "$s" ]; then \
		echo "sfosbuild: image hook $(basename "$s")"; \
		sh "$s"; \
	fi; \
done
