const std = @import("std");
const Allocator = std.mem.Allocator;
const zap = @import("zap");

const N_SHARDS = 64;
const N_THREADS = 4;
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

const LockEndpoint = struct {
    path: []const u8 = "/lock/",
    error_strategy: zap.Endpoint.ErrorStrategy = .log_to_response,

    pub fn post(e: *LockEndpoint, arena: Allocator, context: *LockContext, r: zap.Request) !void {
        _ = e;
        const body = r.body orelse {
            r.setStatus(.bad_request);
            return r.sendBody("missing body");
        };

        // arena is request-scoped — fine for parsing, NOT for storing
        const parsed = std.json.parseFromSlice(
            LockReq,
            arena,
            body,
            .{ .ignore_unknown_fields = true },
        ) catch {
            r.setStatus(.bad_request);
            return r.sendBody("invalid JSON");
        };
        // no defer deinit — arena owns it, cleaned up after handler returns

        const req: LockReq = parsed.value;

        // shard selection
        const h = std.hash.Wyhash.hash(0, req.key);
        const shard = &context.shards[h & (N_SHARDS - 1)];

        const Result = enum { acquired, conflict };
        const result: Result = blk: {
            shard.mutex.lock();
            defer shard.mutex.unlock();

            if (shard.locks.contains(req.key) and req.force != true) break :blk .conflict;

            // dupe into GPA so the strings outlive this request
            const gpa = shard.locks.allocator; // the GPA you passed in at init
            const key_copy = try gpa.dupe(u8, req.key);
            errdefer gpa.free(key_copy);
            const lockee_copy = try gpa.dupe(u8, req.lockee);
            errdefer gpa.free(lockee_copy);

            try shard.locks.put(key_copy, .{
                .key = key_copy,
                .lockee = lockee_copy,
                .since_ns = std.time.nanoTimestamp(),
            });
            break :blk .acquired;
        };

        // Send response AFTER mutex released
        switch (result) {
            .acquired => {
                r.setStatus(.ok);
                const response_body = std.fmt.allocPrint(arena, "{{\"locked\":true,\"key\":\"{s}\",\"lockee\":\"{s}\"}}\n", .{ req.key, req.lockee }) catch {
                    r.setStatus(.internal_server_error);
                    return r.sendBody("failed to format response");
                };
                return r.sendBody(response_body);
            },
            .conflict => {
                r.setStatus(.conflict);
                const response_body = std.fmt.allocPrint(arena, "{{\"locked\":false,\"key\":\"{s}\",\"lockee\":\"{s}\"}}\n", .{ req.key, req.lockee }) catch {
                    r.setStatus(.internal_server_error);
                    return r.sendBody("failed to format response");
                };
                return r.sendBody(response_body);
            },
        }
    }

    pub fn delete(e: *LockEndpoint, arena: Allocator, context: *LockContext, r: zap.Request) !void {
        _ = e;
        const body = r.body orelse {
            r.setStatus(.bad_request);
            return r.sendBody("missing body");
        };

        const parsed = std.json.parseFromSlice(
            LockReq,
            arena,
            body,
            .{ .ignore_unknown_fields = true },
        ) catch {
            r.setStatus(.bad_request);
            return r.sendBody("invalid JSON");
        };

        const req: LockReq = parsed.value;

        const h = std.hash.Wyhash.hash(0, req.key);
        const shard = &context.shards[h & (N_SHARDS - 1)];

        const ReleaseStatus = enum { released, not_found, forbidden };
        const status: ReleaseStatus = blk: {
            shard.mutex.lock();
            defer shard.mutex.unlock();

            const existing = shard.locks.get(req.key);
            if (existing == null) break :blk .not_found;
            if (!std.mem.eql(u8, existing.?.lockee, req.lockee)) break :blk .forbidden;
            _ = shard.locks.remove(req.key);
            shard.locks.allocator.free(existing.?.key);
            shard.locks.allocator.free(existing.?.lockee);
            break :blk .released;
        };

        // Send response AFTER mutex released
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
};

const LocksEndpoint = struct {
    path: []const u8 = "/locks/",
    error_strategy: zap.Endpoint.ErrorStrategy = .log_to_response,

    pub fn get(e: *LocksEndpoint, arena: Allocator, context: *LockContext, r: zap.Request) !void {
        _ = e;
        var all_locks = std.ArrayListUnmanaged(LockState){};
        defer all_locks.deinit(arena);
        for (&context.shards) |*shard| {
            shard.mutex.lock();
            defer shard.mutex.unlock();

            var it = shard.locks.iterator();
            while (it.next()) |kv| {
                const lock = kv.value_ptr.*;
                try all_locks.append(arena, lock);
            }
        }

        var json_buf = std.ArrayListUnmanaged(u8){};
        defer json_buf.deinit(arena);
        try json_buf.appendSlice(arena, "{\"locks\":[");
        for (all_locks.items, 0..) |lock, i| {
            if (i > 0) try json_buf.appendSlice(arena, ",");
            const entry = try std.fmt.allocPrint(arena, "{{\"key\":\"{s}\",\"lockee\":\"{s}\",\"since\":\"{d}\"}}", .{ lock.key, lock.lockee, lock.since_ns });
            try json_buf.appendSlice(arena, entry);
        }
        try json_buf.appendSlice(arena, "]}");

        r.setStatus(.ok);
        try r.sendBody(json_buf.items);
    }
};

const HealthEndpoint = struct {
    path: []const u8 = "/healthz/",
    error_strategy: zap.Endpoint.ErrorStrategy = .log_to_response,

    pub fn get(e: *HealthEndpoint, arena: Allocator, context: *LockContext, r: zap.Request) !void {
        _ = e;
        _ = arena;
        _ = context;

        r.setStatus(.ok);
        try r.sendBody("{\"status\":\"ok\"}\n");
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

    var lockEndpoint = LockEndpoint{};
    var locksEndpoint = LocksEndpoint{};
    var healthEndpoint = HealthEndpoint{};
    try App.register(&lockEndpoint);
    try App.register(&locksEndpoint);
    try App.register(&healthEndpoint);

    try App.listen(.{ .interface = "0.0.0.0", .port = 8080 });

    zap.start(.{ .threads = N_THREADS, .workers = N_WORKERS });
}
