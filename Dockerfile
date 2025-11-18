FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o prometheus-opensearch-datastream-shardguard ./cmd/prometheus-opensearch-datastream-shardguard

FROM alpine:3.20
WORKDIR /app
COPY --from=build /src/prometheus-opensearch-datastream-shardguard /usr/local/bin/prometheus-opensearch-datastream-shardguard

USER 65534:65534
EXPOSE 9108

ENTRYPOINT ["prometheus-opensearch-datastream-shardguard"]
