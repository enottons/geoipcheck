# Build from the repo root: docker build -f deploy/Dockerfile .
#
# Tags are pinned to a minor version here for readability. Production builds
# should pin by digest so a rebuild can't pick up a different base silently.

FROM golang:1.27 AS build
WORKDIR /src

# Dependencies first, so source edits don't invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

# .git is in the context on purpose: the toolchain stamps the commit into
# the binary, which /metrics exposes as geoipcheck_build_info.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/geoipcheck ./cmd/geoipcheck

# static: no libc needed with CGO_ENABLED=0. The nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/geoipcheck /geoipcheck
EXPOSE 8080 9090 8081
ENTRYPOINT ["/geoipcheck"]
