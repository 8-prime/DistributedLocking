const std = @import("std");
const posix = std.posix;
const c = @cImport({
    @cInclude("liburing.h");
});

fn setupListeningSocket(port: u16) !posix.socket_t {
    const sock = try posix.socket(posix.AF.INET, posix.SOCK.STREAM, 0);
    errdefer posix.close(sock);

    const enable: c_int = 1;
    try posix.setsockopt(sock, posix.SOL.SOCKET, posix.SO.REUSEADDR, std.mem.asBytes(&enable));

    const addr = posix.sockaddr.in{
        .port = std.mem.nativeToBig(u16, port),
        .addr = 0, // INADDR_ANY
    };
    try posix.bind(sock, @ptrCast(&addr), @sizeOf(posix.sockaddr.in));
    try posix.listen(sock, 10);

    return sock;
}

pub fn main() !void {
    _ = c;
    std.debug.print("liburing ready\n", .{});
}
