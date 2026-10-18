# batch — подопытная «ночная аналитика»: только stdlib, статический бинарник.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/batch .

# Рантайм: distroless без shell, непривилегированный пользователь — под guardrails из лабы 3.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/batch /batch
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/batch"]
