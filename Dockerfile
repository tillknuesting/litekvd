# A static binary on nothing at all.
#
# litekvd links nothing and shells out to nothing, so the runtime layer needs
# four things: the binary, a CA bundle for a follower that dials an https
# leader, one line of /etc/passwd, and a directory it may write to. That is a
# scratch image with four COPYs, and it is 6.6 MB — 2.8 MB of which used to be
# the zoneinfo database that distroless carries and this never reads, since an
# RFC 3339 timestamp brings its own offset.
#
# There is no shell in it, on purpose and not merely by omission: it is one
# fewer thing to reach for from inside a compromised container, and this
# program has never needed one. It is also why the chart runs two StatefulSets
# rather than branching on a pod ordinal in an entrypoint.

FROM golang:1.26-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# The module first, so the dependency layer survives a change to the source.
# There is one dependency and it is the storage engine.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off makes it static, which is what lets the final stage have no libc.
#
# -trimpath keeps this machine's paths out of the binary. -s drops the symbol
# table and -w the DWARF, together about a third of the size; what they cost is
# a stack trace with no line numbers, which is a trade worth naming rather than
# copying — if you are chasing a panic in production, build without them.
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-arm64} \
    go build -trimpath -ldflags="-s -w" -o /litekvd . \
 && GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-arm64} \
    go build -trimpath -ldflags="-s -w" -o /litekv-controller ./cmd/litekv-controller

# Assembled here because scratch has no shell to assemble them in.
#
# The passwd line is so that a runtime which insists on resolving the uid finds
# a name for it. The directory is because scratch has no writable path at all,
# not even /tmp: without it a container started with no volume cannot create
# its store and exits saying "permission denied", which is a confusing way to
# learn that the image is empty.
RUN printf 'nonroot:x:65532:65532:nonroot:/:/sbin/nologin\n' > /passwd.min \
 && mkdir -p /empty/data

# The controller, which is its own image so that the store's stays as small as
# it is. Build it with --target controller; the store is the default because it
# is the last stage.
#
# It needs the CA bundle for real: it talks to the API server over TLS and
# verifies it against the cluster's own certificate authority.
FROM scratch AS controller

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /passwd.min /etc/passwd
COPY --from=build /litekv-controller /usr/local/bin/litekv-controller

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/litekv-controller"]

FROM scratch AS litekvd

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /passwd.min /etc/passwd
COPY --from=build --chown=65532:65532 /empty/data /data
COPY --from=build /litekvd /usr/local/bin/litekvd

# 65532, which is what distroless calls nonroot and what the chart's fsGroup is
# set to. They have to agree or the volume is not writable.
USER 65532:65532

EXPOSE 8080

# /data is a directory in the image, so a container run with no volume works
# and loses its store when it stops. Mount something there to keep it; the
# chart mounts a PersistentVolumeClaim.
ENTRYPOINT ["/usr/local/bin/litekvd"]
CMD ["-dir=/data", "-addr=0.0.0.0:8080"]
