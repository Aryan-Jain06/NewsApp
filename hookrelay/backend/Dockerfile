# Build stage: compile both binaries from the same module.
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache dependencies before copying the rest of the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api    ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# Runtime stage: distroless-style scratch image with certs for outbound HTTPS.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl \
 && adduser -D -u 10001 hookrelay
COPY --from=build /out/api    /usr/local/bin/api
COPY --from=build /out/worker /usr/local/bin/worker
USER hookrelay
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
