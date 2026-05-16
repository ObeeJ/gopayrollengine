# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download

COPY . .
RUN GOTOOLCHAIN=auto go build -o /app/payroll-app cmd/api/main.go

# Final stage — pinned alpine version for reproducible builds
FROM alpine:3.20

WORKDIR /app

# Create a non-root user — running as root inside a container is a critical
# security finding in any pen test or SOC 2 audit.
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

COPY --from=builder /app/payroll-app .
# Migrations run at startup — the binary needs them at the same relative path.
COPY --from=builder /app/internal/db/migrations ./internal/db/migrations

# Do NOT copy .env into the image — secrets must be injected at runtime
# via environment variables (docker-compose, ECS task definition, K8s secrets, etc.)

# Drop to non-root before the process starts.
USER appuser

EXPOSE 8080

CMD ["./payroll-app"]
