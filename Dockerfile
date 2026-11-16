# Builds one governor binary into a small, non-root image.
#   docker build --build-arg BIN=controlplane -t governor-controlplane .
#   docker build --build-arg BIN=dataplane    -t governor-dataplane .
FROM golang:1.26 AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "$BIN" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/governor ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/governor /governor
COPY config/governor.yaml /etc/governor/governor.yaml
USER nonroot:nonroot
ENTRYPOINT ["/governor", "--config", "/etc/governor/governor.yaml"]
