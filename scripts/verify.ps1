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
    $verifyTempRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot "build/web-verification"))
    Push-Location (Join-Path $repoRoot "web")
    try {
        if ($Mode -eq "full") {
            Write-Host "[Web] Running regression tests"
            & npm test
            if ($LASTEXITCODE -ne 0) {
                throw "Web tests failed with exit code $LASTEXITCODE"
            }
            Write-Host "[Web] Running the production build"
            New-Item -ItemType Directory -Path $verifyTempRoot -Force | Out-Null
            $buildOut = Join-Path $verifyTempRoot ([guid]::NewGuid().ToString("N"))
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
            $resolvedBuildOut = [System.IO.Path]::GetFullPath($buildOut)
            $allowedPrefix = $verifyTempRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
            if (-not $resolvedBuildOut.StartsWith($allowedPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
                throw "Refusing to remove a build path outside the verification directory"
            }
            Remove-Item -LiteralPath $resolvedBuildOut -Recurse -Force
        }
    }
}

Invoke-GoVerification
if (-not $BackendOnly) {
    Invoke-WebVerification
}
Write-Host "Verification passed (mode=$Mode)"
