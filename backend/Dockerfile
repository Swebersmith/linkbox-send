FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/linkbox ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates sqlite tzdata && addgroup -g 10001 linkbox && adduser -D -H -u 10001 -G linkbox linkbox
COPY --from=build /out/linkbox /usr/local/bin/linkbox
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/linkbox"]
