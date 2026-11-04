# The relay (go/cmd/relay). Build from the module, go/: docker build -f cmd/relay/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/relay"]
