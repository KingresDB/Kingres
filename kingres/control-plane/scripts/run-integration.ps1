# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs the integration tests of the control plane inside WSL2 against the
# private KingresDB installation in ~/kw-cp/install. kingresd and its tests are
# built for Linux on Windows (GOOS=linux go test -c), copied into WSL, and run
# there by run-wsl-tests.sh; the instances the tests start (through the
# instance manager itself) use throwaway directories under ~/kw-cp/test and
# a port slice of its own (20000-27999, see run-wsl-tests.sh) and are removed afterwards; leftovers of
# a killed run are removed by the next run, by proof of ownership only.
#
# Usage (from kingres\control-plane):
#   .\scripts\run-integration.ps1 [-Packages ./internal/instance,./internal/sysdb] [-Run TestName] [-Race] [-Install ~/kw-cp19/install] [-Lane 2]
#
# -Packages takes one package, or several as separate array elements (-Packages ./a, ./b); one
# string with commas inside is not split.
#
# Lanes: runs of the same lane queue up behind one mutex; runs of different lanes run side by
# side. A lane has its own staging directory and its own directory of binaries in WSL
# (~/kw-cp/test/bin-<lane>); the run roots and the port slices of run-wsl-tests.sh already keep
# concurrent runs apart. Give each lane its own installation (-Install) when the extensions
# differ, since an installation is shared by every run that names it. No -Lane is the default
# lane, the one every run used before lanes existed.
param(
    [string[]]$Packages = @(),
    [string]$Run = '',
    [string]$Distro = 'Ubuntu-24.04',
    [string]$WslUser = 'kingres',
    [switch]$Race,
    # The PostgreSQL installation the instances are made from, as a path inside WSL (~ and $HOME
    # are expanded there); it reaches the tests as KINGRES_INSTALL. Empty keeps the default of
    # run-wsl-tests.sh (~/kw-cp/install, PostgreSQL 18); an installation of PostgreSQL 19 runs
    # the same suite on that major.
    [string]$Install = '',
    # Count runs every selected test that many times in the same binary (stress loops); each run
    # makes its own clusters. Any failure fails the run.
    [int]$Count = 1,
    # A directory (a Windows path) where the tests keep the whole publication of a real cluster (KINGRES_PUBLICATION_FIXTURES inside WSL).
    [string]$Fixtures = '',
    # Further variables for the tests, NAME=value with plain characters, separated by semicolons (for example KINGRES_QUIET_MINUTES=10).
    [string]$TestEnv = '',
    # The lane of the run (lower-case letters and digits); empty is the default lane.
    [string]$Lane = ''
)
$ErrorActionPreference = 'Stop'
if ($Lane -notmatch '^[a-z0-9]*$') { throw "-Lane must be lower-case letters and digits, got '$Lane'" }
$laneSuffix = ''
if ($Lane) { $laneSuffix = "-$Lane" }
# Two runs would share the staging directory, ~/kw-cp/test/bin and the test ports: runs of
# several people or agents on one machine queue up behind this mutex instead.
$lock = New-Object System.Threading.Mutex($false, "Global\kingres-cp-integration$laneSuffix")
try { [void]$lock.WaitOne() } catch [System.Threading.AbandonedMutexException] { }
$module = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Push-Location $module
try {
    if ($Packages.Count -eq 0) {
        # Every package that has test files.
        # Listed for linux: the tests run there, and some packages have test files that only exist for it.
        $env:GOOS = 'linux'
        try { $Packages = @(& go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | Where-Object { $_ }) } finally { Remove-Item Env:GOOS -ErrorAction SilentlyContinue }
    }
    function ConvertTo-WslPath([string]$p) { $p = [IO.Path]::GetFullPath($p); return '/mnt/' + $p.Substring(0,1).ToLower() + $p.Substring(2).Replace([string][char]92, '/') }
    $stage = Join-Path ([IO.Path]::GetTempPath()) "kingresd-wsl-tests$laneSuffix"
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
    # The lease watchdog (tiny, next to the instance), once it exists.
    if (Test-Path (Join-Path $module "cmd/kingres-watchdog")) {
        & go build -o (Join-Path $stage "kingres-watchdog") ./cmd/kingres-watchdog
        if ($LASTEXITCODE -ne 0) { throw "go build kingres-watchdog failed" }
    }
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
    & wsl.exe -d $Distro -u $WslUser bash -c "rm -rf ~/kw-cp/test/bin$laneSuffix && mkdir -p ~/kw-cp/test/bin$laneSuffix && cp '$wslStage'/kingresd '$wslStage'/kingres-verify '$wslStage'/*.test '$wslStage'/openapi.yaml '$wslStage'/README.md '$wslStage'/operations_gen.go.txt '$wslStage'/wsl-allowed-skips.txt ~/kw-cp/test/bin$laneSuffix/ && cp -r '$wslStage'/fixtures ~/kw-cp/test/bin$laneSuffix/ && chmod +x ~/kw-cp/test/bin$laneSuffix/*.test ~/kw-cp/test/bin$laneSuffix/kingresd ~/kw-cp/test/bin$laneSuffix/kingres-verify && { if [ -f '$wslStage'/kingres-watchdog ]; then cp '$wslStage'/kingres-watchdog ~/kw-cp/test/bin$laneSuffix/ && chmod +x ~/kw-cp/test/bin$laneSuffix/kingres-watchdog; fi; }"
    if ($Install -and $Install -notmatch '^[A-Za-z0-9_./~$-]+$') { throw "-Install must be a plain path inside WSL, got '$Install'" }
    $envInstall = "export KINGRES_TEST_BIN=`$HOME/kw-cp/test/bin$laneSuffix; "
    if ($Install) { $envInstall += "export KINGRES_INSTALL=$Install; " }
    if ($Fixtures) { New-Item -ItemType Directory -Force $Fixtures | Out-Null; $envInstall += "export KINGRES_PUBLICATION_FIXTURES='$(ConvertTo-WslPath $Fixtures)'; " }
    foreach ($kv in ($TestEnv -split ';' | Where-Object { $_ })) {
        if ($kv -notmatch '^[A-Z][A-Z0-9_]*=[A-Za-z0-9_./-]*$') { throw "-TestEnv holds a pair that is not NAME=value with plain characters: $kv" }
        $envInstall += "export $kv; "
    }
    $bins = ($names | ForEach-Object { "`$HOME/kw-cp/test/bin$laneSuffix/$_.test" }) -join ' '
    $flags = @()
    if ($Run) { $flags += "-test.run '$Run'" }
    if ($Count -gt 1) { $flags += "-test.count=$Count" }
    $extra = ''
    if ($flags.Count -gt 0) { $extra = "-- " + ($flags -join ' ') }
    # Output goes to a file: server processes started by the tests may
    # inherit the pipe and keep PowerShell waiting for its end.
    $log = Join-Path $stage 'run.log'
    & wsl.exe -d $Distro -u $WslUser bash -c "${envInstall}bash '$wslScript' $bins $extra" *> $log
    $exit = $LASTEXITCODE
    Get-Content $log
    exit $exit
} finally {
    Pop-Location
    $lock.ReleaseMutex()
}
