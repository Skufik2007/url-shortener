# syntax=docker/dockerfile:1

# ---- сборка ----
FROM golang:1.27-alpine AS build

WORKDIR /src

# Сначала только манифесты модулей: слой с зависимостями кэшируется,
# пока не меняются go.mod / go.sum. go.sum может отсутствовать, пока нет внешних зависимостей.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN mkdir -p /out \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shortener ./cmd/shortener

# ---- рантайм ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/shortener /shortener

USER nonroot

EXPOSE 8080

ENTRYPOINT ["/shortener"]
