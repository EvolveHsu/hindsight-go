FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/hindsight-go ./cmd/hindsight-go

FROM scratch
COPY --from=build /bin/hindsight-go /bin/hindsight-go
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8890
ENTRYPOINT ["/bin/hindsight-go"]
