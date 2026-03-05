using System.Text;
using System.Threading.Channels;
using WyHash;

namespace dotnet_inmemory;

public struct LockEntry
{
    public required string Key { get; set; }
    public required string Lockee { get; set; }
    public DateTime Since { get; set; }
}

public interface ILockMessage
{
}

public struct LockMessage : ILockMessage
{
    public string LockId { get; set; }
    public string Lockee { get; set; }
    public bool Force { get; set; }
    public TaskCompletionSource<LockResponse> TaskCompletionSource { get; set; }
}

public struct UnlockMessage : ILockMessage
{
    public string LockId { get; set; }
    public string Lockee { get; set; }
    public TaskCompletionSource<UnlockResult> TaskCompletionSource { get; set; }
}

public struct GetLocksMessage : ILockMessage
{
    public TaskCompletionSource<AllLocks> TaskCompletionSource { get; set; }
}

public class LockService : BackgroundService
{
    private Dictionary<string, LockEntry> _locks = new Dictionary<string, LockEntry>();

    private readonly Channel<ILockMessage> _channel =
        Channel.CreateBounded<ILockMessage>(new BoundedChannelOptions(100));

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        await foreach (var message in _channel.Reader.ReadAllAsync(stoppingToken))
        {
            switch (message)
            {
                case LockMessage lockMessage:
                    if (!_locks.TryGetValue(lockMessage.LockId, out var lockEntry))
                    {
                        _locks[lockMessage.LockId] = new LockEntry
                        {
                            Key = lockMessage.LockId,
                            Lockee = lockMessage.Lockee,
                            Since = DateTime.UtcNow
                        };
                        lockMessage.TaskCompletionSource.SetResult(new LockResponse
                        {
                            Lockee = lockMessage.Lockee,
                            Key = lockMessage.LockId,
                            Locked = true
                        });
                        continue;
                    }

                    if (lockEntry.Lockee == lockMessage.Lockee)
                    {
                        lockMessage.TaskCompletionSource.SetResult(new LockResponse
                        {
                            Lockee = lockMessage.Lockee,
                            Key = lockMessage.LockId,
                            Locked = true
                        });
                        continue;
                    }

                    if (lockMessage.Force)
                    {
                        _locks[lockMessage.LockId] = new LockEntry
                        {
                            Key = lockMessage.LockId,
                            Lockee = lockMessage.Lockee,
                            Since = DateTime.UtcNow
                        };
                        lockMessage.TaskCompletionSource.SetResult(new LockResponse
                        {
                            Lockee = lockMessage.Lockee,
                            Key = lockMessage.LockId,
                            Locked = true
                        });
                        continue;
                    }

                    lockMessage.TaskCompletionSource.SetResult(new LockResponse
                    {
                        Lockee = lockEntry.Lockee,
                        Key = lockEntry.Key,
                        Locked = false
                    });
                    break;
                case UnlockMessage unlockMessage:
                    if (!_locks.TryGetValue(unlockMessage.LockId, out var unlockEntry))
                    {
                        unlockMessage.TaskCompletionSource.SetResult(UnlockResult.NotFound);
                        continue;
                    }

                    if (unlockEntry.Lockee == unlockMessage.Lockee)
                    {
                        _locks.Remove(unlockMessage.LockId);
                        unlockMessage.TaskCompletionSource.SetResult(UnlockResult.Released);
                        continue;
                    }

                    unlockMessage.TaskCompletionSource.SetResult(UnlockResult.Disallowed);
                    break;
                case GetLocksMessage getLocksMessage:
                    getLocksMessage.TaskCompletionSource.SetResult(new AllLocks
                    {
                        Locks = _locks.Values.ToArray()
                    });
                    break;
            }
        }
    }

    public async Task<LockResponse> Lock(LockRequest request)
    {
        var msg = new LockMessage
        {
            Lockee = request.Lockee,
            LockId = request.Key,
            Force = request.Force ?? false,
            TaskCompletionSource = new TaskCompletionSource<LockResponse>(),
        };
        await _channel.Writer.WriteAsync(msg);
        return await msg.TaskCompletionSource.Task;
    }

    public async Task<UnlockResult> Unlock(LockRequest request)
    {
        var msg = new UnlockMessage
        {
            Lockee = request.Lockee,
            LockId = request.Key,
            TaskCompletionSource = new TaskCompletionSource<UnlockResult>()
        };
        await _channel.Writer.WriteAsync(msg);
        return await msg.TaskCompletionSource.Task;        
    }

    public async Task<AllLocks> GetAllLocks()
    {
        var msg = new GetLocksMessage
        {
            TaskCompletionSource = new TaskCompletionSource<AllLocks>()
        };
        await _channel.Writer.WriteAsync(msg);
        return await msg.TaskCompletionSource.Task;
    }
}