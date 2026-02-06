FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/llm-core ./cmd/core

FROM alpine:3.20
WORKDIR /app
LABEL org.opencontainers.image.title="llmcore" \
      org.opencontainers.image.description="LLM-MCP core service" \
      org.opencontainers.image.source="https://github.com/plagness/LLM-MCP.git" \
      ns.module="llm" \
      ns.component="core"
COPY --from=build /out/llm-core /usr/local/bin/llm-core
EXPOSE 8080
CMD ["/usr/local/bin/llm-core"]
