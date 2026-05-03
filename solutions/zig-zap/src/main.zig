const std = @import("std");
const Allocator = std.mem.Allocator;
const zap = @import("zap");

const N_SHARDS = 256;
const N_WORKERS = 1;

const LockState = struct {
    key: []const u8,
    lockee: []const u8,
    since_ns: i128,
};

const Shard = struct {
    mutex: std.Thread.Mutex,
    locks: std.StringHashMap(LockState),
};

const LockReq = struct {
    key: []const u8,
    lockee: []const u8,
    force: ?bool = null,
};

const LockContext = struct {
    shards: [N_SHARDS]Shard,

    pub fn init(allocator: std.mem.Allocator) LockContext {
        return .{
            .shards = [_]Shard{.{ .mutex = std.Thread.Mutex{}, .locks = std.StringHashMap(LockState).init(allocator) }} ** N_SHARDS,
        };
    }
};

fn fmtRfc3339(ns: i128, arena: Allocator) ![]const u8 {
    const secs: u64 = @intCast(@divTrunc(ns, std.time.ns_per_s));
    const epoch_secs = std.time.epoch.EpochSeconds{ .secs = secs };
    const day_secs = epoch_secs.getDaySeconds();
    const epoch_day = epoch_secs.getEpochDay();
    const year_day = epoch_day.calculateYearDay();
    const month_day = year_day.calculateMonthDay();
    return std.fmt.allocPrint(arena, "{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}Z", .{
        @as(u32, year_day.year),
        @as(u32, month_day.month.numeric()),
        @as(u32, month_day.day_index) + 1,
        @as(u32, day_secs.getHoursIntoDay()),
        @as(u32, day_secs.getMinutesIntoHour()),
        @as(u32, day_secs.getSecondsIntoMinute()),
    });
}

// Matches path exactly, tolerating an optional trailing slash on actual.
fn matchPath(actual: []const u8, expected: []const u8) bool {
    if (std.mem.eql(u8, actual, expected)) return true;
    return actual.len == expected.len + 1 and
        actual[actual.len - 1] == '/' and
        std.mem.eql(u8, actual[0..expected.len], expected);
}

fn handleLock(arena: Allocator, ctx: *LockContext, r: zap.Request) !void {
    const body = r.body orelse {
        r.setStatus(.bad_request);
        return r.sendBody("missing body");
    };

    const parsed = std.json.parseFromSlice(LockReq, arena, body, .{ .ignore_unknown_fields = true }) catch {
        r.setStatus(.bad_request);
        return r.sendBody("invalid JSON");
    };
    const req = parsed.value;

    const h = std.hash.Wyhash.hash(0, req.key);
    const shard = &ctx.shards[h & (N_SHARDS - 1)];

    const ResultTag = enum { acquired, conflict };
    const Result = struct { tag: ResultTag, current_lockee: []const u8 };

    const result: Result = blk: {
        shard.mutex.lock();
        defer shard.mutex.unlock();

        const gpa = shard.locks.allocator;

        if (shard.locks.get(req.key)) |existing| {
            if (std.mem.eql(u8, existing.lockee, req.lockee)) {
                break :blk .{ .tag = .acquired, .current_lockee = existing.lockee };
            }
            if (req.force != true) {
                break :blk .{ .tag = .conflict, .current_lockee = existing.lockee };
            }
            if (shard.locks.fetchRemove(req.key)) |old| {
                gpa.free(old.key);
                gpa.free(old.value.lockee);
            }
        }

        const key_copy = try gpa.dupe(u8, req.key);
        errdefer gpa.free(key_copy);
        const lockee_copy = try gpa.dupe(u8, req.lockee);
        errdefer gpa.free(lockee_copy);

        try shard.locks.put(key_copy, .{
            .key = key_copy,
            .lockee = lockee_copy,
            .since_ns = std.time.nanoTimestamp(),
        });
        break :blk .{ .tag = .acquired, .current_lockee = lockee_copy };
    };

    switch (result.tag) {
        .acquired => {
            r.setStatus(.ok);
            const resp = try std.fmt.allocPrint(arena, "{{\"locked\":true,\"key\":\"{s}\",\"lockee\":\"{s}\"}}\n", .{ req.key, req.lockee });
            return r.sendBody(resp);
        },
        .conflict => {
            r.setStatus(.conflict);
            const resp = try std.fmt.allocPrint(arena, "{{\"locked\":false,\"key\":\"{s}\",\"currentLockee\":\"{s}\"}}\n", .{ req.key, result.current_lockee });
            return r.sendBody(resp);
        },
    }
}

fn handleUnlock(arena: Allocator, ctx: *LockContext, r: zap.Request) !void {
    const body = r.body orelse {
        r.setStatus(.bad_request);
        return r.sendBody("missing body");
    };

    const parsed = std.json.parseFromSlice(LockReq, arena, body, .{ .ignore_unknown_fields = true }) catch {
        r.setStatus(.bad_request);
        return r.sendBody("invalid JSON");
    };
    const req = parsed.value;

    const h = std.hash.Wyhash.hash(0, req.key);
    const shard = &ctx.shards[h & (N_SHARDS - 1)];

    const ReleaseStatus = enum { released, not_found, forbidden };
    const status: ReleaseStatus = blk: {
        shard.mutex.lock();
        defer shard.mutex.unlock();

        const existing = shard.locks.get(req.key);
        if (existing == null) break :blk .not_found;
        if (!std.mem.eql(u8, existing.?.lockee, req.lockee)) break :blk .forbidden;
        if (shard.locks.fetchRemove(req.key)) |old| {
            shard.locks.allocator.free(old.key);
            shard.locks.allocator.free(old.value.lockee);
        }
        break :blk .released;
    };

    switch (status) {
        .released => r.setStatus(.ok),
        .not_found => {
            r.setStatus(.not_found);
            return r.sendBody("not found");
        },
        .forbidden => {
            r.setStatus(.forbidden);
            return r.sendBody("lockee mismatch");
        },
    }
}

fn handleListLocks(arena: Allocator, ctx: *LockContext, r: zap.Request) !void {
    var all_locks = std.ArrayListUnmanaged(LockState){};
    defer all_locks.deinit(arena);
    for (&ctx.shards) |*shard| {
        shard.mutex.lock();
        defer shard.mutex.unlock();
        var it = shard.locks.iterator();
        while (it.next()) |kv| {
            try all_locks.append(arena, kv.value_ptr.*);
        }
    }

    var buf = std.ArrayListUnmanaged(u8){};
    defer buf.deinit(arena);
    try buf.appendSlice(arena, "{\"locks\":[");
    for (all_locks.items, 0..) |lock, i| {
        if (i > 0) try buf.appendSlice(arena, ",");
        const since_str = try fmtRfc3339(lock.since_ns, arena);
        const entry = try std.fmt.allocPrint(arena, "{{\"key\":\"{s}\",\"lockee\":\"{s}\",\"since\":\"{s}\"}}", .{ lock.key, lock.lockee, since_str });
        try buf.appendSlice(arena, entry);
    }
    try buf.appendSlice(arena, "]}");

    r.setStatus(.ok);
    try r.sendBody(buf.items);
}

// Single dispatcher endpoint at "/" — avoids EndpointPathShadowError that fires
// when /lock and /locks are registered separately (one is a prefix of the other).
const Dispatcher = struct {
    path: []const u8 = "/",
    error_strategy: zap.Endpoint.ErrorStrategy = .log_to_response,

    pub fn get(self: *Dispatcher, arena: Allocator, ctx: *LockContext, r: zap.Request) !void {
        _ = self;
        const path = r.path orelse return;
        if (matchPath(path, "/healthz")) {
            r.setStatus(.ok);
            return r.sendBody("{\"status\":\"ok\"}\n");
        } else if (matchPath(path, "/locks")) {
            return handleListLocks(arena, ctx, r);
        }
        r.setStatus(.not_found);
        try r.sendBody("not found");
    }

    pub fn post(self: *Dispatcher, arena: Allocator, ctx: *LockContext, r: zap.Request) !void {
        _ = self;
        const path = r.path orelse return;
        if (matchPath(path, "/lock")) {
            return handleLock(arena, ctx, r);
        } else if (matchPath(path, "/unlock")) {
            return handleUnlock(arena, ctx, r);
        }
        r.setStatus(.not_found);
        try r.sendBody("not found");
    }
};

pub fn main() !void {
    var gpa: std.heap.GeneralPurposeAllocator(.{
        .thread_safe = true,
    }) = .{};
    defer std.debug.print("\n\nLeaks detected: {}\n\n", .{gpa.deinit() != .ok});
    const allocator = gpa.allocator();
    const App = zap.App.Create(LockContext);
    var lockContext = LockContext.init(allocator);
    try App.init(allocator, &lockContext, .{});
    defer App.deinit();

    var dispatcher = Dispatcher{};
    try App.register(&dispatcher);

    try App.listen(.{ .interface = "0.0.0.0", .port = 8080 });

    const cpu_count = std.Thread.getCpuCount() catch 4;
    zap.start(.{ .threads = @intCast(cpu_count), .workers = N_WORKERS });
}
