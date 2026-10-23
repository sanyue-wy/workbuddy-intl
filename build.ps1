# WorkBuddy 国际版多平台跨平台编译脚本 (PowerShell)
param(
    [string]$Version = "1.0.0"
)

$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $scriptDir

# 如果存在本地 .gocache 则配置缓存
if (Test-Path "$scriptDir\.gocache") {
    $env:GOCACHE = "$scriptDir\.gocache"
}

$distDir = "$scriptDir\dist"
if (-not (Test-Path $distDir)) {
    New-Item -ItemType Directory -Path $distDir | Out-Null
}

$env:CGO_ENABLED = "0"

$targets = @(
    @{ os = "windows"; arch = "amd64"; ext = ".exe" },
    @{ os = "windows"; arch = "arm64"; ext = ".exe" },
    @{ os = "linux";   arch = "amd64"; ext = "" },
    @{ os = "linux";   arch = "arm64"; ext = "" },
    @{ os = "darwin";  arch = "amd64"; ext = "" },
    @{ os = "darwin";  arch = "arm64"; ext = "" }
)

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host " WorkBuddy 国际版 (workbuddy-intl) 多平台二进制编译开始 v$Version" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan

foreach ($t in $targets) {
    $out = "$distDir\workbuddy-intl-$($t.os)-$($t.arch)$($t.ext)"
    Write-Host "[BUILD] 正在构建 $($t.os)/$($t.arch) -> $out ..." -ForegroundColor Green
    $env:GOOS = $t.os
    $env:GOARCH = $t.arch
    go build -ldflags="-s -w -X main.version=$Version" -trimpath -o $out main.go
}

Write-Host "`n[CHECKSUM] 计算 SHA256 校验和..." -ForegroundColor Yellow
$checksumFile = "$distDir\checksums.txt"
if (Test-Path $checksumFile) { Remove-Item $checksumFile }

$files = Get-ChildItem -Path $distDir -File | Where-Object { $_.Name -ne "checksums.txt" }
foreach ($f in $files) {
    $hash = (Get-FileHash -Path $f.FullName -Algorithm SHA256).Hash.ToLower()
    $line = "$hash  $($f.Name)"
    Add-Content -Path $checksumFile -Value $line
    Write-Host "  $line"
}

Write-Host "`n==========================================================" -ForegroundColor Cyan
Write-Host " 构建完成！全部 6 大平台纯净二进制均已存入 dist/ 目录" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
