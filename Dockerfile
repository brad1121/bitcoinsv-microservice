FROM golang:1.25-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY --from=sdk . /src/bitcoinsv-sdk-go

WORKDIR /src/bsvms
COPY . .
RUN CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bsvms ./cmd/bsvms
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blackjack ./cmd/blackjack

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/bsvms /usr/local/bin/bsvms
COPY --from=build /out/blackjack /usr/local/bin/blackjack
EXPOSE 50051
ENTRYPOINT ["/usr/local/bin/bsvms"]
