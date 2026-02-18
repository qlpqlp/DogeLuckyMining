# DogeLuckyMining - build binaries for Windows, Linux, macOS
# Run in PowerShell: .\build.ps1

$ErrorActionPreference = "Stop"
$dist = "dist"
if (Test-Path $dist) { Remove-Item -Recurse -Force $dist }
New-Item -ItemType Directory -Path $dist | Out-Null

$targets = @(
    @{ GOOS = "linux";   GOARCH = "amd64"; out = "dogelucky-linux-amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; out = "dogelucky-linux-arm64" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; out = "dogelucky-darwin-amd64" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; out = "dogelucky-darwin-arm64" },
    @{ GOOS = "windows"; GOARCH = "amd64"; out = "dogelucky-windows-amd64.exe" },
    @{ GOOS = "windows"; GOARCH = "386";   out = "dogelucky-windows-386.exe" }
)

foreach ($t in $targets) {
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    Write-Host "Building $($t.out)..."
    go build -ldflags "-s -w" -o "$dist\$($t.out)" .
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue

# Generate SHA256 checksums for verification (so users can confirm binaries were not modified)
$sumsPath = Join-Path $dist "SHA256SUMS.txt"
$sums = Get-ChildItem $dist -File | ForEach-Object {
    $hash = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
    "$hash  $($_.Name)"
}
$sums | Set-Content -Path $sumsPath -Encoding UTF8
Write-Host "Done. Binaries and SHA256SUMS.txt in $dist\"
