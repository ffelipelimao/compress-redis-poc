FROM golang:1.24-alpine AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/poc ./cmd/poc

FROM alpine:3.20
COPY --from=build /out/poc /usr/local/bin/poc
ENTRYPOINT ["poc"]
