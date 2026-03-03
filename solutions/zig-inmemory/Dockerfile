FROM alpine:3.21 AS builder
RUN apk add --no-cache curl xz
RUN curl -fsSL https://ziglang.org/download/0.15.2/zig-linux-x86_64-0.15.2.tar.xz \
    | tar -xJ && mv zig-linux-x86_64-0.15.2 /zig
ENV PATH="/zig:$PATH"
WORKDIR /app
COPY . .
RUN zig build -Doptimize=ReleaseFast -Dtarget=x86_64-linux-musl

FROM scratch
COPY --from=builder /app/zig-out/bin/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
