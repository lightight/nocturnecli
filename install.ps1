# Nocturne installer (Windows, PowerShell).
#   irm https://nocturnecli.lol/install.ps1 | iex
#
# Env overrides:
#   NOCTURNE_REPO         GitHub owner/repo for the build fallback
#   NOCTURNE_INSTALL_DIR  where to put the binary (default %LOCALAPPDATA%\Nocturne\bin)

$ErrorActionPreference = "Stop"

$Base = "__BASE__" # replaced by the server with its own URL when served
$Repo = if ($env:NOCTURNE_REPO) { $env:NOCTURNE_REPO } else { "lightight/nocturnecli" }
$InstallDir = if ($env:NOCTURNE_INSTALL_DIR) { $env:NOCTURNE_INSTALL_DIR } else { "$env:LOCALAPPDATA\Nocturne\bin" }

Write-Host "* Nocturne installer" -ForegroundColor Yellow
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$Dest = Join-Path $InstallDir "nocturne.exe"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$asset = "nocturne_windows_$arch.exe"
$ok = $false

# 1) prebuilt binary served by the host
if ($Base -like "http*://*") {
  try {
    Write-Host "-> downloading $Base/bin/$asset" -ForegroundColor Blue
    Invoke-WebRequest -Uri "$Base/bin/$asset" -OutFile $Dest -UseBasicParsing
    if ((Get-Item $Dest).Length -gt 0) { $ok = $true }
  } catch { }
}

# 2) GitHub release
if (-not $ok) {
  try {
    $url = "https://github.com/$Repo/releases/latest/download/$asset"
    Write-Host "-> downloading $url" -ForegroundColor Blue
    Invoke-WebRequest -Uri $url -OutFile $Dest -UseBasicParsing
    if ((Get-Item $Dest).Length -gt 0) { $ok = $true }
  } catch { }
}

# 3) build with Go
if (-not $ok -and (Get-Command go -ErrorAction SilentlyContinue)) {
  Write-Host "no prebuilt binary - building with go install" -ForegroundColor DarkGray
  $env:GOBIN = $InstallDir
  go install "github.com/$Repo@latest"
  if ($LASTEXITCODE -eq 0) { $ok = $true }
}

if (-not $ok) { Write-Error "Could not install. Install Go (https://go.dev/dl) and re-run."; exit 1 }
Write-Host "Installed $Dest" -ForegroundColor Green

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$InstallDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
  Write-Host "Added $InstallDir to your PATH - restart your terminal." -ForegroundColor Yellow
}
Write-Host ""
Write-Host "Set your key:  `$env:NOCTURNE_API='noct_your_key'" -ForegroundColor DarkGray
Write-Host "Then run:      nocturne" -ForegroundColor Yellow
