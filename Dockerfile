FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY benchmark/go.mod benchmark/go.sum ./
RUN go mod download
COPY benchmark/ ./
RUN go build -o /benchmark .

FROM alpine:latest
WORKDIR /app
COPY --from=builder /benchmark ./benchmark
COPY solutions/ ./solutions/
# Results are written to ../results relative to WORKDIR, i.e. /results.
# Mount a volume at /results to persist output.
# The Docker socket must be mounted so the runner can build and start solution containers.
VOLUME /results
ENTRYPOINT ["./benchmark"]
CMD ["solutions/"]
