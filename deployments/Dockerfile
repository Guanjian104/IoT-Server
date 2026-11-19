FROM golang:1.27.3-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=cs
ARG REVISION=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X iot-server/internal/buildinfo.Version=${VERSION} -X iot-server/internal/buildinfo.Revision=${REVISION}" \
    -o /out/server ./cmd/server

FROM debian:bookworm-slim
COPY --from=build /out/server /usr/local/bin/server
USER 65534:65534
ENTRYPOINT ["server"]
