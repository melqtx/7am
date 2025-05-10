FROM golang:1.24 as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY web ./web

RUN CGO_ENABLED=0 GOOS=linux go build -o ./server

FROM gcr.io/distroless/base-debian11 AS build-release-stage

COPY --from=builder /app/server /app/server

USER nonroot:nonroot

WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["./server"]
