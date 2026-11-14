# SPDX-License-Identifier: PostgreSQL
#
# Runs the integration tests of the control plane inside WSL2 against the
# private KingresDB installation in ~/kw-cp/install. kingresd and its tests are
# built for Linux on Windows (GOOS=linux go test -c), copied into WSL, and run
# there by run-wsl-tests.sh; the instances the tests start (through the
# instance manager itself) use throwaway directories under ~/kw-cp/test and
# ports 56000-56199 and are removed afterwards.
#
# Usage (from kingres\control-plane):
#   .\scripts\run-integration.ps1 [-Packages ./internal/instance,./internal/sysdb] [-Run TestName] [-Race]
param(
    [string[]]$Packages = @(),
    [string]$Run = '',
    [string]$Distro = 'Ubuntu-24.04',
    [string]$WslUser = 'kingres',
    [switch]$Race
)
$ErrorActionPreference = 'Stop'
$module = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Push-Location $module
try {
    if ($Packages.Count -eq 0) {
        # Every package that has test files.
        $Packages = @(& go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | Where-Object { $_ })
    }
    function ConvertTo-WslPath([string]$p) { $p = [IO.Path]::GetFullPath($p); return '/mnt/' + $p.Substring(0,1).ToLower() + $p.Substring(2).Replace([string][char]92, '/') }
    $stage = Join-Path ([IO.Path]::GetTempPath()) 'kingresd-wsl-tests'
    $stage = [IO.Path]::GetFullPath($stage)
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory $stage | Out-Null
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'
    $names = @()
    foreach ($p in $Packages) {
        $name = ($p -replace '[^A-Za-z0-9]+', '_')
        $out = Join-Path $stage "$name.test"
        $flags = @('test', '-c', '-o', $out)
        if ($Race) { Write-Warning 'the race detector needs cgo for linux; build it inside WSL instead'; }
        & go @flags $p
        if ($LASTEXITCODE -ne 0) { throw "go test -c $p failed" }
        $names += $name
    }
    # kingresd itself for the multi-server tests (they start several processes).
    & go build -o (Join-Path $stage 'kingresd') ./cmd/kingresd
    if ($LASTEXITCODE -ne 0) { throw 'go build kingresd failed' }
    # The standalone verifier (kingres/verifier, standard library only): the proof tests feed it
    # what the API returned (KINGRES_VERIFY_BIN, exported by run-wsl-tests.sh).
    Push-Location (Join-Path (Split-Path $module -Parent) 'verifier')
    try { & go build -o (Join-Path $stage 'kingres-verify') ./cmd/kingres-verify } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'go build kingres-verify failed' }
    Remove-Item Env:GOOS, Env:GOARCH
    # Fixtures and the contract the tests read.
    New-Item -ItemType Directory (Join-Path $stage 'fixtures') | Out-Null
    Copy-Item (Join-Path $PSScriptRoot 'fixture_*.sql') (Join-Path $stage 'fixtures')
    Copy-Item (Join-Path $module 'internal/contract/operations_gen.go') (Join-Path $stage 'operations_gen.go.txt')
    Copy-Item (Join-Path $PSScriptRoot 'wsl-allowed-skips.txt') (Join-Path $stage 'wsl-allowed-skips.txt')
    Copy-Item (Join-Path (Split-Path $module -Parent) 'api/openapi.yaml') (Join-Path $stage 'openapi.yaml')
    Copy-Item (Join-Path $module 'README.md') (Join-Path $stage 'README.md')
    # Copy into WSL so that the binaries run from the native file system.
    $wslStage = ConvertTo-WslPath $stage
    $wslScript = ConvertTo-WslPath (Join-Path $PSScriptRoot 'run-wsl-tests.sh')
    & wsl.exe -d $Distro -u $WslUser bash -c "rm -rf ~/kw-cp/test/bin && mkdir -p ~/kw-cp/test/bin && cp '$wslStage'/kingresd '$wslStage'/kingres-verify '$wslStage'/*.test '$wslStage'/openapi.yaml '$wslStage'/README.md '$wslStage'/operations_gen.go.txt '$wslStage'/wsl-allowed-skips.txt ~/kw-cp/test/bin/ && cp -r '$wslStage'/fixtures ~/kw-cp/test/bin/ && chmod +x ~/kw-cp/test/bin/*.test ~/kw-cp/test/bin/kingresd ~/kw-cp/test/bin/kingres-verify"
    $bins = ($names | ForEach-Object { "`$HOME/kw-cp/test/bin/$_.test" }) -join ' '
    $extra = ''
    if ($Run) { $extra = "-- -test.run '$Run'" }
    # Output goes to a file: server processes started by the tests may
    # inherit the pipe and keep PowerShell waiting for its end.
    $log = Join-Path $stage 'run.log'
    & wsl.exe -d $Distro -u $WslUser bash -c "bash '$wslScript' $bins $extra" *> $log
    $exit = $LASTEXITCODE
    Get-Content $log
    exit $exit
} finally {
    Pop-Location
}
