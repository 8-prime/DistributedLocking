FROM alpine:3.21 AS builder
RUN apk add --no-cache curl xz
RUN curl -fsSL https://ziglang.org/download/0.15.2/zig-x86_64-linux-0.15.2.tar.xz \
    | tar -xJ && mv zig-x86_64-linux-0.15.2 /zig
ENV PATH="/zig:$PATH"
WORKDIR /app
COPY . .
RUN zig build -Doptimize=ReleaseFast -Dtarget=x86_64-linux-musl
# Copy libfacil.io.so for the final stage to be able to run the application in a minimal image
RUN find / -name "libfacil.io.so" 2>/dev/null -exec cp {} /app/libfacil.io.so \;

FROM alpine:3.21
COPY --from=builder /app/zig-out/bin/zig_zap /zig_zap
COPY --from=builder /app/libfacil.io.so /usr/local/lib/libfacil.io.so
EXPOSE 8080
ENTRYPOINT ["/zig_zap"]
