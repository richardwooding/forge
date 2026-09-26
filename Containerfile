# Release image, built by GoReleaser (dockers_v2). The binaries are staged in
# the build context as <os>/<arch>/forge, hence the $TARGETPLATFORM copy.
#
# The Go toolchain is in the image on purpose. forge's whole function is to
# compile Go source to WebAssembly, and `forge tool add` shells out to the real
# toolchain to do it; an image without Go would be a forge that can run tools
# somebody else built and nothing more. It costs a few hundred megabytes, which
# is the honest price of the feature.
FROM cgr.dev/chainguard/wolfi-base:latest

ARG TARGETPLATFORM

RUN apk add --no-cache \
    go ca-certificates-bundle git \
 && adduser -D -h /home/forge -u 1000 forge \
 && mkdir -p /home/forge/.local/share/forge /home/forge/.cache/forge /home/forge/.config/forge \
 && chown -R forge:forge /home/forge

COPY $TARGETPLATFORM/forge /usr/local/bin/forge

USER forge
WORKDIR /home/forge
ENV HOME=/home/forge \
    XDG_DATA_HOME=/home/forge/.local/share \
    XDG_CACHE_HOME=/home/forge/.cache \
    XDG_CONFIG_HOME=/home/forge/.config \
    # forge pins GOTOOLCHAIN=local for tool builds so that a tool's go.mod
    # cannot make the go command download and run a different toolchain. The
    # image's Go is therefore the Go every tool built here is compiled with.
    GOTOOLCHAIN=local

ENTRYPOINT ["forge"]
