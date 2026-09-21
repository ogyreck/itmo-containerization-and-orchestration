# Сборка: зависимости подтягиваются go mod tidy, бинарник статический.
FROM golang:1-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod ./
COPY main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/api .

# Рантайм: distroless без shell и пакетного менеджера, запуск от непривилегированного пользователя.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]
