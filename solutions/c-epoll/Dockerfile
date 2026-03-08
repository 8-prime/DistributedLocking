FROM alpine:3.21 AS builder
RUN apk add --no-cache clang musl-dev linux-headers
WORKDIR /app
COPY server.c .
RUN clang -O3 -march=x86-64-v3 -mtune=generic -flto \
    -fomit-frame-pointer -fno-stack-protector \
    -DNDEBUG \
    -static \
    -o server server.c \
    -lpthread

FROM scratch
COPY --from=builder /app/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
