# Один Dockerfile на оба сервиса: что собирать, выбирается через --build-arg SERVICE=api|worker,
# версия зашивается в бинарник через VERSION (её видно в /version и заголовке X-App-Version).
FROM golang:1.27-alpine AS build
ARG SERVICE=api
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/ogyreck/shop/internal/common.Version=${VERSION}" \
    -o /out/app ./cmd/${SERVICE}

# Рантайм: distroless без shell и пакетного менеджера, запуск от непривилегированного пользователя.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/app"]
