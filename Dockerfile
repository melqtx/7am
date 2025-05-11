FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY prompt.txt ./
COPY web ./web

RUN CGO_ENABLED=0 GOOS=linux go build -o ./server

FROM alpine:3.21.3

ARG uid
ARG gid
RUN apk add --no-cache tzdata && addgroup -g ${gid} -S nonroot && adduser -u ${uid} -S nonroot -G nonroot

COPY --from=builder --chown=nonroot:nonroot /app/server /app/server
WORKDIR /app
RUN chown -R nonroot:nonroot /app

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["./server"]
