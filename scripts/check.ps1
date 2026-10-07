# Stage-0 build pipeline for hindsight-go.
#
# Steps:
#   1. normalize the upstream OpenAPI 3.1 spec (TagGroup/Content unions, nullable headers)
#   2. regenerate internal/api with ogen (optional with -Regenerate; skipped otherwise)
#   3. go build ./... && go vet ./... && go test ./...
#   4. assert the generated Handler interface matches the spec operation-for-operation
#
# Usage:
#   pwsh scripts\check.ps1
#   pwsh scripts\check.ps1 -Regenerate

param(
    [switch]$Regenerate
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$repo = Split-Path -Parent (Split-Path -Parent $root)   # project checkout root

$goCandidates = @(
    $env:GO,
    (Get-Command go -ErrorAction SilentlyContinue).Source
) | Where-Object { $_ -and (Test-Path -LiteralPath $_) }
if (-not $goCandidates) { throw 'go.exe not found; set GO or add Go to PATH' }
$go = $goCandidates[0]

$pyCandidates = @(
    $env:PYTHON,
    (Get-Command python -ErrorAction SilentlyContinue).Source
) | Where-Object { $_ -and (Test-Path -LiteralPath $_) }
if (-not $pyCandidates) { throw 'python.exe not found; set PYTHON or add Python to PATH' }
$py = $pyCandidates[0]

Write-Host '== hindsight-go check ==' -ForegroundColor Cyan
Write-Host "go : $go"
Write-Host "py : $py"

# 1. normalize
Write-Host "`n[1/4] normalize upstream spec" -ForegroundColor Cyan
$upstream = Join-Path $repo 'hindsight\hindsight-docs\static\openapi.json'
if (-not (Test-Path -LiteralPath $upstream)) { throw "upstream spec missing: $upstream" }
Push-Location (Join-Path $repo 'hindsight-go\probe')
try {
    & $py 'normalize2.py' | ForEach-Object { Write-Host "  $_" }
    if ($LASTEXITCODE -ne 0) { throw 'normalize2.py failed' }
    Copy-Item 'openapi.go.json' (Join-Path $root 'openapi.go.json') -Force
} finally { Pop-Location }

# 2. generate (opt-in; committed code is the default build input)
if ($Regenerate) {
    Write-Host "`n[2/4] regenerate internal/api" -ForegroundColor Cyan
    $ogen = Join-Path $env:USERPROFILE 'go\bin\ogen.exe'
    if (-not (Test-Path -LiteralPath $ogen)) { throw "ogen.exe missing: $ogen (run: go install github.com/ogen-go/ogen/cmd/ogen@v1.24.0)" }
    Push-Location $root
    try {
        & $ogen -clean -config ogen.yml -target internal\api -package api openapi.go.json | ForEach-Object { Write-Host "  $_" }
        if ($LASTEXITCODE -ne 0) { throw 'ogen failed' }
    } finally { Pop-Location }
} else {
    Write-Host "`n[2/4] regenerate internal/api" -ForegroundColor DarkGray
    Write-Host '  skipped (use -Regenerate to rebuild internal/api from the spec)'
}

# 3. build / vet / test
Write-Host "`n[3/4] build + vet + test" -ForegroundColor Cyan
Push-Location $root
try {
    & $go build ./...
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    Write-Host '  build OK'

    & $go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    Write-Host '  vet OK'

    & $go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
    Write-Host '  test OK'
} finally { Pop-Location }

# 4. operation coverage assert: spec ops == Handler methods
Write-Host "`n[4/4] operation coverage assert" -ForegroundColor Cyan
Push-Location $root
try {
    & $py 'scripts\coverage_assert.py' | ForEach-Object { Write-Host "  $_" }
    if ($LASTEXITCODE -ne 0) { throw 'coverage assert failed' }
} finally { Pop-Location }

Write-Host "`nAll checks passed." -ForegroundColor Green
