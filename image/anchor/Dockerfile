# Minimal anchor image. Single statically-linked Go binary in scratch.
# No shell, no package manager, no agent — if this image is ever
# compromised, there is nothing on it to leverage.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags '-s -w' \
      -o /out/anchor main.go

FROM scratch
COPY --from=build /out/anchor /anchor
ENTRYPOINT ["/anchor"]
