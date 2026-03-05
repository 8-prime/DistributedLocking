FROM mcr.microsoft.com/dotnet/sdk:10.0 AS dotnet-publish

WORKDIR /src

COPY src/ ./
RUN rm -rf ./obj ./bin
RUN ls -l

RUN dotnet publish ./dotnet-inmemory.csproj \
    -c Release \
    -o /app/publish

FROM mcr.microsoft.com/dotnet/aspnet:10.0

WORKDIR /app
COPY --from=dotnet-publish /app/publish .

ENTRYPOINT ["dotnet", "dotnet-inmemory.dll"]