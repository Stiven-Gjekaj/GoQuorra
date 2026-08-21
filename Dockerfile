# The image that runs any of the three binaries.
#
# One image and not three. They share almost every byte of their dependencies,
# so three images cost three copies of the same layers, and a deployment then
# has to keep three tags in step.

FROM golang:1.24-alpine AS build

WORKDIR /src

# The manifests first, and on their own. This layer is rebuilt only when a
# dependency changes, so an edit to the source does not download the module
# cache again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off, so the binaries are static and the runtime image needs no C
# library. Trimpath, so the paths of the build machine are not written into
# them.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/ ./cmd/...

# A separate stage for the schema, so the runtime image can apply it without
# carrying the whole source tree.
FROM alpine:3.20

# ca-certificates for any handler that talks to an address on the internet.
# tzdata because a job scheduled by a human is scheduled in a human's zone,
# and a container with no zone database reads every one of them as UTC.
RUN apk add --no-cache ca-certificates tzdata

# A user that is not root. A process that is compromised should not also own
# the filesystem it is standing on.
RUN adduser -D -u 10001 quorra

COPY --from=build /out/quorra-server /usr/local/bin/quorra-server
COPY --from=build /out/quorra-worker /usr/local/bin/quorra-worker
COPY --from=build /out/quorractl /usr/local/bin/quorractl
COPY migrations /migrations

USER quorra

EXPOSE 8080 50051

# CMD and not ENTRYPOINT, and the difference is not cosmetic.
#
# This image holds three binaries, so the caller has to be able to choose one.
# The two orchestrators spell that choice with the same word and mean
# different things by it: `command:` in Kubernetes replaces the entrypoint,
# and `command:` in Docker Compose replaces CMD. With an entrypoint set, a
# compose service asking for the worker ran the server with the path of the
# worker as an argument instead. The server takes no arguments, so it ignored
# it and started, and the two worker containers restarted for ever with an
# error about a missing API key that they have no reason to need.
#
# With CMD alone, both orchestrators select the binary the same way. The
# binaries also refuse an argument they do not understand now, so the same
# mistake stops the container instead of quietly starting the wrong program.
#
# There is no default API key. The image before this one carried a key that
# was printed in the README, and the server refuses to start without one.
CMD ["/usr/local/bin/quorra-server"]
