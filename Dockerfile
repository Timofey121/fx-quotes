FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/fx-quotes ./cmd/fx-quotes

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/fx-quotes /usr/local/bin/fx-quotes
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fx-quotes"]
