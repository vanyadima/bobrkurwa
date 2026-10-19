FROM golang:alpine AS builder
WORKDIR /app
ENV GOTOOLCHAIN=auto
COPY main.go .
RUN go mod init koderline && \
    go get github.com/gorilla/websocket && \
    go get modernc.org/sqlite && \
    CGO_ENABLED=0 GOOS=linux go build -o server main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/server .
COPY static ./static
VOLUME ["/app/data"]
EXPOSE 8080
CMD ["./server"]