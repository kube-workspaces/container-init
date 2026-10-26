# The native CI matrix (or make build-all) supplies verified static binaries.
FROM alpine:3.22
ARG TARGETARCH
COPY --chmod=0755 bin/linux-${TARGETARCH}/container-init /usr/local/bin/container-init
ENTRYPOINT ["/usr/local/bin/container-init"]
