const std = @import("std");

// ── Sharded lock store ───────────────────────────────────────────────────────

const SHARD_COUNT: usize = 64;

const LockEntry = struct {
    key: []const u8,
    lockee: []const u8,
    since_ns: i128,
};

const Shard = struct {
    rwlock: std.Thread.RwLock = .{},
    map: std.StringHashMapUnmanaged(LockEntry) = .{},
    gpa: std.heap.GeneralPurposeAllocator(.{}) = .{},

    fn allocator(self: *Shard) std.mem.Allocator {
        return self.gpa.allocator();
    }
};

const AcquireResult = union(enum) {
    acquired,
    conflict: []const u8, // conflict lockee, duped into arena, owned by caller
};

const ReleaseStatus = enum { ok, not_found, forbidden };

const LockStore = struct {
    shards: [SHARD_COUNT]Shard,

    fn init() LockStore {
        return .{ .shards = [_]Shard{.{}} ** SHARD_COUNT };
    }

    fn shardFor(self: *LockStore, key: []const u8) *Shard {
        const h: usize = @truncate(std.hash.Wyhash.hash(0, key));
        return &self.shards[h % SHARD_COUNT];
    }

    /// arena: allocator for the returned conflict lockee slice (safe to use after lock released)
    fn acquire(
        self: *LockStore,
        key: []const u8,
        lockee: []const u8,
        force: bool,
        arena: std.mem.Allocator,
    ) !AcquireResult {
        const shard = self.shardFor(key);
        const a = shard.allocator();

        shard.rwlock.lock();
        defer shard.rwlock.unlock();

        if (shard.map.get(key)) |existing| {
            if (std.mem.eql(u8, existing.lockee, lockee)) {
                // Idempotent re-acquire: same lockee, preserve since_ns.
                return .acquired;
            }
            if (!force) {
                // Copy lockee into arena while lock is held (safe to use after release)
                return .{ .conflict = try arena.dupe(u8, existing.lockee) };
            }
            // Force overwrite: remove old entry first, then free its strings
            _ = shard.map.remove(key);
            a.free(existing.key);
            a.free(existing.lockee);
        }

        const key_copy = try a.dupe(u8, key);
        errdefer a.free(key_copy);
        const lockee_copy = try a.dupe(u8, lockee);
        errdefer a.free(lockee_copy);

        try shard.map.put(a, key_copy, .{
            .key = key_copy,
            .lockee = lockee_copy,
            .since_ns = std.time.nanoTimestamp(),
        });
        return .acquired;
    }

    fn release(self: *LockStore, key: []const u8, lockee: []const u8) ReleaseStatus {
        const shard = self.shardFor(key);
        const a = shard.allocator();

        shard.rwlock.lock();
        defer shard.rwlock.unlock();

        const entry = shard.map.get(key) orelse return .not_found;
        if (!std.mem.eql(u8, entry.lockee, lockee)) return .forbidden;

        _ = shard.map.remove(key);
        a.free(entry.key);
        a.free(entry.lockee);
        return .ok;
    }

    /// Returns a slice of LockEntry owned by out_alloc (arena).
    fn list(self: *LockStore, out_alloc: std.mem.Allocator) ![]LockEntry {
        var entries: std.ArrayList(LockEntry) = .{};
        for (&self.shards) |*shard| {
            shard.rwlock.lockShared();
            defer shard.rwlock.unlockShared();
            var it = shard.map.valueIterator();
            while (it.next()) |entry| {
                try entries.append(out_alloc, .{
                    .key = try out_alloc.dupe(u8, entry.key),
                    .lockee = try out_alloc.dupe(u8, entry.lockee),
                    .since_ns = entry.since_ns,
                });
            }
        }
        return entries.toOwnedSlice(out_alloc);
    }
};

// ── RFC 3339 timestamp formatting ────────────────────────────────────────────

/// Format since_ns (i128 from std.time.nanoTimestamp) as "YYYY-MM-DDTHH:MM:SSZ".
/// Uses Howard Hinnant's civil-time algorithm for Y/M/D.
fn formatRfc3339(since_ns: i128, buf: []u8) []u8 {
    const secs_i128 = @divTrunc(since_ns, 1_000_000_000);
    const secs: i64 = @intCast(secs_i128);

    // Split into whole days + seconds-within-day using floor division (correct for negatives)
    const days: i64 = @divFloor(secs, 86400);
    const day_secs: i64 = secs - days * 86400; // [0, 86399]

    const hour: u64 = @intCast(@divTrunc(day_secs, 3600));
    const minute: u64 = @intCast(@divTrunc(@mod(day_secs, 3600), 60));
    const second: u64 = @intCast(@mod(day_secs, 60));

    // Hinnant civil-from-days algorithm
    const z: i64 = days + 719468;
    const era: i64 = @divFloor(z, 146097);
    const doe: u64 = @intCast(z - era * 146097); // [0, 146096]
    const yoe: u64 = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365; // [0, 399]
    const y: i64 = @as(i64, @intCast(yoe)) + era * 400;
    const doy: u64 = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    const mp: u64 = (5 * doy + 2) / 153; // [0, 11]
    const d: u64 = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    const m: u64 = if (mp < 10) mp + 3 else mp - 9; // [1, 12]
    const year: i64 = y + @as(i64, if (m <= 2) 1 else 0);

    return std.fmt.bufPrint(buf, "{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}Z", .{
        year, m, d, hour, minute, second,
    }) catch buf[0..0];
}

// ── HTTP connection handler ───────────────────────────────────────────────────

const ConnContext = struct {
    store: *LockStore,
};

fn handleConn(conn: std.net.Server.Connection, ctx: *ConnContext) void {
    defer conn.stream.close();

    // Per-connection arena backed by page_allocator (thread-safe, no shared GPA)
    var arena = std.heap.ArenaAllocator.init(std.heap.page_allocator);
    defer arena.deinit();

    var read_buf: [8192]u8 = undefined;
    var write_buf: [8192]u8 = undefined;
    var conn_reader = conn.stream.reader(&read_buf);
    var conn_writer = conn.stream.writer(&write_buf);
    var http_server = std.http.Server.init(conn_reader.interface(), &conn_writer.interface);

    while (http_server.reader.state == .ready) {
        var req = http_server.receiveHead() catch break;
        handleRequest(&req, ctx.store, arena.allocator()) catch {};
        _ = arena.reset(.retain_capacity);
    }
}

fn handleRequest(
    req: *std.http.Server.Request,
    store: *LockStore,
    alloc: std.mem.Allocator,
) !void {
    const method = req.head.method;
    const raw_target = req.head.target;
    // Strip trailing slash for uniform matching (except root "/").
    const target = if (raw_target.len > 1 and raw_target[raw_target.len - 1] == '/')
        raw_target[0 .. raw_target.len - 1]
    else
        raw_target;

    if (std.mem.eql(u8, target, "/healthz") and method == .GET) {
        return sendJson(req, 200, "{\"status\":\"ok\"}\n");
    }
    if (std.mem.eql(u8, target, "/locks") and method == .GET) {
        return handleList(req, store, alloc);
    }
    if (std.mem.eql(u8, target, "/lock") and method == .POST) {
        return handleAcquire(req, store, alloc);
    }
    if (std.mem.eql(u8, target, "/unlock") and method == .POST) {
        return handleRelease(req, store, alloc);
    }
    return sendStatus(req, 404);
}

fn readBody(req: *std.http.Server.Request, alloc: std.mem.Allocator) ![]u8 {
    var body_buf: [8192]u8 = undefined;
    const body_reader = try req.readerExpectContinue(&body_buf);
    return body_reader.allocRemaining(alloc, .limited(65536));
}

fn handleAcquire(
    req: *std.http.Server.Request,
    store: *LockStore,
    alloc: std.mem.Allocator,
) !void {
    const body = try readBody(req, alloc);

    const Req = struct {
        key: []const u8,
        lockee: []const u8,
        force: ?bool = null,
    };
    const parsed = std.json.parseFromSlice(Req, alloc, body, .{
        .ignore_unknown_fields = true,
    }) catch {
        return sendStatus(req, 400);
    };
    defer parsed.deinit();
    const p = parsed.value;

    if (p.key.len == 0 or p.lockee.len == 0) {
        return sendStatus(req, 400);
    }

    const result = store.acquire(p.key, p.lockee, p.force orelse false, alloc) catch {
        return sendStatus(req, 500);
    };

    var out: std.ArrayList(u8) = .{};
    const w = out.writer(alloc);
    switch (result) {
        .acquired => {
            try w.writeAll("{\"locked\":true,\"key\":");
            try writeJsonString(w, p.key);
            try w.writeAll(",\"lockee\":");
            try writeJsonString(w, p.lockee);
            try w.writeAll("}\n");
            return sendJson(req, 200, out.items);
        },
        .conflict => |current| {
            try w.writeAll("{\"locked\":false,\"key\":");
            try writeJsonString(w, p.key);
            try w.writeAll(",\"currentLockee\":");
            try writeJsonString(w, current);
            try w.writeAll("}\n");
            return sendJson(req, 409, out.items);
        },
    }
}

fn handleRelease(
    req: *std.http.Server.Request,
    store: *LockStore,
    alloc: std.mem.Allocator,
) !void {
    const body = try readBody(req, alloc);

    const Req = struct {
        key: []const u8,
        lockee: []const u8,
    };
    const parsed = std.json.parseFromSlice(Req, alloc, body, .{
        .ignore_unknown_fields = true,
    }) catch {
        return sendStatus(req, 400);
    };
    defer parsed.deinit();
    const p = parsed.value;

    if (p.key.len == 0 or p.lockee.len == 0) {
        return sendStatus(req, 400);
    }

    return sendStatus(req, switch (store.release(p.key, p.lockee)) {
        .ok => 200,
        .not_found => 404,
        .forbidden => 403,
    });
}

fn handleList(
    req: *std.http.Server.Request,
    store: *LockStore,
    alloc: std.mem.Allocator,
) !void {
    const entries = try store.list(alloc);

    var out: std.ArrayList(u8) = .{};
    const w = out.writer(alloc);
    try w.writeAll("{\"locks\":[");

    var ts_buf: [32]u8 = undefined;
    for (entries, 0..) |entry, i| {
        if (i > 0) try w.writeByte(',');
        const ts = formatRfc3339(entry.since_ns, &ts_buf);
        try w.writeByte('{');
        try w.writeAll("\"key\":");
        try writeJsonString(w, entry.key);
        try w.writeAll(",\"lockee\":");
        try writeJsonString(w, entry.lockee);
        try w.writeAll(",\"since\":\"");
        try w.writeAll(ts);
        try w.writeAll("\"}");
    }
    try w.writeAll("]}\n");

    return sendJson(req, 200, out.items);
}

// ── Helpers ──────────────────────────────────────────────────────────────────

fn writeJsonString(w: anytype, s: []const u8) !void {
    try w.writeByte('"');
    for (s) |c| {
        switch (c) {
            '"' => try w.writeAll("\\\""),
            '\\' => try w.writeAll("\\\\"),
            '\n' => try w.writeAll("\\n"),
            '\r' => try w.writeAll("\\r"),
            '\t' => try w.writeAll("\\t"),
            else => try w.writeByte(c),
        }
    }
    try w.writeByte('"');
}

fn sendJson(req: *std.http.Server.Request, status: u10, body: []const u8) !void {
    try req.respond(body, .{
        .status = @enumFromInt(status),
        .extra_headers = &.{
            .{ .name = "content-type", .value = "application/json" },
        },
    });
}

fn sendStatus(req: *std.http.Server.Request, status: u10) !void {
    try req.respond("", .{
        .status = @enumFromInt(status),
    });
}

// ── Main ─────────────────────────────────────────────────────────────────────

pub fn main() !void {
    var store = LockStore.init();

    const addr = try std.net.Address.parseIp4("0.0.0.0", 8080);
    var net_server = try addr.listen(.{
        .reuse_address = true,
    });
    defer net_server.deinit();

    var gpa = std.heap.GeneralPurposeAllocator(.{}){};
    defer _ = gpa.deinit();
    const alloc = gpa.allocator();

    var pool: std.Thread.Pool = undefined;
    try pool.init(.{ .allocator = alloc });
    defer pool.deinit();

    const ctx = try alloc.create(ConnContext);
    defer alloc.destroy(ctx);
    ctx.* = .{ .store = &store };

    while (true) {
        const conn = net_server.accept() catch continue;
        pool.spawn(handleConn, .{ conn, ctx }) catch {
            conn.stream.close();
        };
    }
}
