# Deploy tender-monitor to Dokploy from Windows
# Usage: `$env:DOKPLOY_URL="https://..."; `$env:DOKPLOY_TOKEN="..."; .\deploy.ps1

$ErrorActionPreference = "Stop"
$DOKPLOY_URL = $env:DOKPLOY_URL
$DOKPLOY_TOKEN = $env:DOKPLOY_TOKEN
$APP_NAME = if ($env:APP_NAME) { $env:APP_NAME } else { "tender-monitor" }

if (-not $DOKPLOY_URL) { throw "DOKPLOY_URL not set" }
if (-not $DOKPLOY_TOKEN) { throw "DOKPLOY_TOKEN not set" }

Write-Host "=== Deploying $APP_NAME to $DOKPLOY_URL ===" -ForegroundColor Cyan

# 1. Create app
$body = @{ name = $APP_NAME; type = "application" } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri "$DOKPLOY_URL/api/application.create" -Method Post -Headers @{ Authorization = "Bearer $DOKPLOY_TOKEN" } -ContentType "application/json" -Body $body
$APP_ID = $resp.applicationId
Write-Host "App created: $APP_ID" -ForegroundColor Green

# 2. Deploy
$body = @{ applicationId = $APP_ID } | ConvertTo-Json
Invoke-RestMethod -Uri "$DOKPLOY_URL/api/application.deploy" -Method Post -Headers @{ Authorization = "Bearer $DOKPLOY_TOKEN" } -ContentType "application/json" -Body $body

Write-Host "Deploy triggered. Check $DOKPLOY_URL for status." -ForegroundColor Green