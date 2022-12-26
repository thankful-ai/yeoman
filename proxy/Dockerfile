FROM golang:1.19-alpine3.17
WORKDIR /opt/app
COPY . /opt/app
RUN go build -o app ./cmd/proxy/main.go

FROM alpine:latest
WORKDIR /app
COPY --from=0 /opt/app ./
CMD ["./app"]
