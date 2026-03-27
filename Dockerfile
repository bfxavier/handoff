FROM golang:1.26-alpine AS build
WORKDIR /app
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ .
RUN CGO_ENABLED=0 GOOS=linux go build -o handoff .

FROM alpine:3.21
RUN apk add --no-cache curl ca-certificates
WORKDIR /app
COPY --from=build /app/handoff .
COPY public/ public/
EXPOSE 3000
CMD ["./handoff"]
