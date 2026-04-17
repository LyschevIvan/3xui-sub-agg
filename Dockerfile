FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -o /out/aggregator ./cmd/aggregator

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/aggregator /app/aggregator
EXPOSE 8080
ENTRYPOINT ["/app/aggregator", "-config", "/app/config.yaml"]
