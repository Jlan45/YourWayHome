FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/your-way-home .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/your-way-home /usr/local/bin/your-way-home
COPY frpc.ini /app/frpc.ini

ENTRYPOINT ["/usr/local/bin/your-way-home"]
CMD ["-c", "/app/frpc.ini"]
