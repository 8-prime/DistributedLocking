namespace dotnet_inmemory;

public struct LockRequest
{
    public required string Key { get; init; }
    public required string Lockee { get; init; }
    public bool? Force { get; init; }
}