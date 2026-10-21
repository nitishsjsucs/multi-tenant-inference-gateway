# syntax=docker/dockerfile:1
# Builds all four binaries; the default entrypoint is the gateway.
FROM golang:1.24.7-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY config/gateway.yaml /etc/mtig/gateway.yaml
COPY prompts/ /etc/mtig/prompts/
WORKDIR /etc/mtig
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/etc/mtig/gateway.yaml"]
