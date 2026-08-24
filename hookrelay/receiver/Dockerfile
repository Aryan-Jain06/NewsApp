FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/receiver .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl \
 && adduser -D -u 10002 receiver
COPY --from=build /out/receiver /usr/local/bin/receiver
USER receiver
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/receiver"]
