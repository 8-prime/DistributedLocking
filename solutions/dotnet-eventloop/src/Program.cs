using dotnet_inmemory;
using Microsoft.AspNetCore.Http.HttpResults;
using Microsoft.AspNetCore.Mvc;

var builder = WebApplication.CreateBuilder(args);

builder.Services.AddSingleton<LockService>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<LockService>());

var app = builder.Build();

app.MapPost("/lock", LockHandler);
app.MapPost("/unlock", UnLockHandler);
app.MapGet("/locks", GetLocksHandler);
app.MapGet("/healthz", () => TypedResults.Text("{\"status\": \"ok\"}", "application/json"));

await app.RunAsync();
return;

static async Task<Results<Ok<LockResponse>, Conflict<LockResponse>>> LockHandler(
    [FromBody] LockRequest request,
    [FromServices] LockService lockService
)
{
    var result = await lockService.Lock(request);
    if (!result.Locked)
    {
        return TypedResults.Conflict(result);
    }

    return TypedResults.Ok(result);
}

static async Task<Results<Ok, NotFound, ForbidHttpResult>> UnLockHandler(
    [FromBody] LockRequest request,
    [FromServices] LockService lockService
)
{
    var result = await lockService.Unlock(request);
    return result switch
    {
        UnlockResult.Released => TypedResults.Ok(),
        UnlockResult.NotFound => TypedResults.NotFound(),
        UnlockResult.Disallowed => TypedResults.Forbid(),
        _ => throw new ArgumentOutOfRangeException(),
    };
}

static async Task<Ok<AllLocks>> GetLocksHandler([FromServices] LockService lockService)
{
    var result = await lockService.GetAllLocks();
    return TypedResults.Ok(result);
}