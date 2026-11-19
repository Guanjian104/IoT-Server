FROM docker.m.daocloud.io/library/golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=cs
ARG REVISION=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X iot-server/internal/buildinfo.Version=${VERSION} -X iot-server/internal/buildinfo.Revision=${REVISION}" \
    -o /out/server ./cmd/server

FROM docker.m.daocloud.io/library/debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
COPY --from=build /out/server /usr/local/bin/server
USER 65534:65534
ENTRYPOINT ["server"]
