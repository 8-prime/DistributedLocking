using System.Text.Json.Serialization;

namespace dotnet_inmemory;

public struct LockResponse
{
    public required bool Locked { get; init; }
    public required string Key { get; init; }
    public required string Lockee { get; set; }
}