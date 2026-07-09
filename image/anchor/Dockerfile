# Minimal anchor image. Single statically-linked Go binary in scratch.
# No shell, no package manager, no agent — if this image is ever
# compromised, there is nothing on it to leverage.

# Pinned by digest for a reproducible build, matching the sandbox image's
# policy (the anchor's build toolchain shouldn't float while the main image's
# is pinned). Bump deliberately: re-pull golang:1.26-alpine, read its digest,
# update here.
FROM golang:1.26-alpine@sha256:f1ddd9fe14fffc091dd98cb4bfa999f32c5fc77d2f2305ea9f0e2595c5437c14 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags '-s -w' \
      -o /out/anchor main.go

FROM scratch
COPY --from=build /out/anchor /anchor
ENTRYPOINT ["/anchor"]
