[CmdletBinding()]
param(
    [ValidateSet("quick", "full")]
    [string]$Mode = "quick",
    [string]$Run = "",
    [switch]$BackendOnly
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot

function Invoke-GoVerification {
    Push-Location (Join-Path $repoRoot "server")
    try {
        if ($Run) {
            Write-Host "[Go] Running targeted service tests: $Run"
            & go test ./service -run $Run -count=1
        }
        elseif ($Mode -eq "full") {
            Write-Host "[Go] Running the full test suite"
            & go test ./... -count=1
        }
        else {
            Write-Host "[Go] Compiling all packages without running tests"
            & go test ./... -run '^$' -count=1
        }
        if ($LASTEXITCODE -ne 0) {
            throw "Go verification failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
}

function Invoke-WebVerification {
    $buildOut = ""
    Push-Location (Join-Path $repoRoot "web")
    try {
        if ($Mode -eq "full") {
            Write-Host "[Web] Running the production build"
            $buildOut = Join-Path ([System.IO.Path]::GetTempPath()) ("quantvista-web-verify-" + [guid]::NewGuid().ToString("N"))
            New-Item -ItemType Directory -Path $buildOut | Out-Null
            & npm run build -- --outDir $buildOut
        }
        else {
            Write-Host "[Web] Running type checks"
            & npm run type-check
        }
        if ($LASTEXITCODE -ne 0) {
            throw "Web verification failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
        if ($buildOut -and (Test-Path -LiteralPath $buildOut)) {
            Remove-Item -LiteralPath $buildOut -Recurse -Force
        }
    }
}

Invoke-GoVerification
if (-not $BackendOnly) {
    Invoke-WebVerification
}
Write-Host "Verification passed (mode=$Mode)"
