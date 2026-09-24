FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/fx-quotes-fixture ./test/fixture/provider/cmd

FROM alpine:3.22
COPY --from=build /out/fx-quotes-fixture /usr/local/bin/fx-quotes-fixture
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/fx-quotes-fixture"]
