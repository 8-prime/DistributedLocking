using System.Text;
// using WyHash;

namespace dotnet_inmemory;

public struct LockEntry
{
    public required string LockId { get; set; }
    public required string Lockee { get; set; }
    public DateTime Since { get; set; }
}

public class LockShard
{
    public SemaphoreSlim Semaphore { get; } = new SemaphoreSlim(1, 1);
    public Dictionary<string, LockEntry> Locks { get; set; } = new Dictionary<string, LockEntry>();
}

public class LockService
{
    private const int ShardCount = 64;
    private readonly LockShard[] _shards;

    public LockService()
    {
        _shards = new LockShard[64];
        for (var i = 0; i < ShardCount; i++)
        {
            _shards[i] = new LockShard();
        }
    }

    public async Task<LockResponse> Lock(LockRequest lockRequest)
    {
        // var shard = _shards[WyHash64.ComputeHash64(Encoding.UTF8.GetBytes(lockRequest.Key)) & (ShardCount - 1)];
        var shard = _shards[lockRequest.Key.GetHashCode() & (ShardCount - 1)];
        await shard.Semaphore.WaitAsync();
        try
        {
            if (shard.Locks.TryGetValue(lockRequest.Key, out var existingLock) &&
                existingLock.Lockee != lockRequest.Lockee)
            {
                return new LockResponse
                {
                    Key = lockRequest.Key,
                    Lockee = existingLock.Lockee,
                    Locked = false,
                };
            }

            shard.Locks[lockRequest.Key] = new LockEntry
            {
                Lockee = lockRequest.Lockee,
                LockId = lockRequest.Key,
                Since = DateTime.UtcNow
            };
            return new LockResponse
            {
                Key = lockRequest.Key,
                Lockee = lockRequest.Lockee,
                Locked = true,
            };
        }
        finally
        {
            shard.Semaphore.Release();
        }
    }

    public async Task<UnlockResult> Unlock(LockRequest unlockRequest)
    {
        // var shard = _shards[WyHash64.ComputeHash64(Encoding.UTF8.GetBytes(unlockRequest.Key)) & (ShardCount - 1)];
        var shard = _shards[unlockRequest.Key.GetHashCode() & (ShardCount - 1)];
        await shard.Semaphore.WaitAsync();
        try
        {
            if (!shard.Locks.TryGetValue(unlockRequest.Key, out var existingLock))
            {
                return UnlockResult.NotFound;
            }

            if (existingLock.Lockee != unlockRequest.Lockee)
            {
                return UnlockResult.Disallowed;
            }

            shard.Locks.Remove(unlockRequest.Key);
            return UnlockResult.Released;
        }
        finally
        {
            shard.Semaphore.Release();
        }
    }
    
    public async Task<AllLocks> GetAllLocks()
    {
        var allLocks = new List<LockEntry>();
        foreach (var shard in _shards)
        {
            await shard.Semaphore.WaitAsync();
            try
            {
                allLocks.AddRange(shard.Locks.Values);
            }
            finally
            {
                shard.Semaphore.Release();
            }
        }

        return new AllLocks { Locks = allLocks };
    }
}