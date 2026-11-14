# SPDX-License-Identifier: PostgreSQL
#
# Runs the whole test suite of the control plane, including the PostgreSQL
# integration tests, against a throwaway KingresDB build in WSL2: starts a
# primary and a streaming standby on ports in 56000-56099 (see
# pg-test-instance.sh), runs "go test" from Windows with KINGRES_TEST_DSN set,
# and always stops and removes the instances afterwards.
#
# Usage (from kingres\control-plane):  .\scripts\run-integration.ps1 [-Port 56010] [-Race]
param(
    [int]$Port = 56010,
    [string]$Distro = 'Ubuntu-24.04',
    [string]$WslUser = 'kingres',
    [switch]$Race
)
$ErrorActionPreference = 'Stop'
if ($Port -lt 56000 -or $Port + 1 -gt 56099) { throw "port $Port is outside 56000-56099" }

$script = (Resolve-Path (Join-Path $PSScriptRoot 'pg-test-instance.sh')).Path
$wslScript = (& wsl.exe -d $Distro -u $WslUser wslpath -a $script).Trim()
$standby = $Port + 1

function Invoke-Instance([string]$action) {
    # Output goes to a file: the server processes inherit the pipe of a
    # direct invocation and would keep PowerShell waiting for its end.
    $out = New-TemporaryFile
    try {
        & wsl.exe -d $Distro -u $WslUser bash $wslScript $action $Port $standby *> $out.FullName
        if ($LASTEXITCODE -ne 0) { Get-Content $out.FullName; throw "pg-test-instance.sh $action failed" }
        Get-Content $out.FullName
    } finally { Remove-Item $out.FullName -ErrorAction SilentlyContinue }
}

$exit = 1
try {
    Invoke-Instance 'start'
    $env:KINGRES_TEST_DSN = "postgres://cp_service:cp_service_pw@127.0.0.1:$Port/kingres_cp?sslmode=disable"
    $goArgs = @('test', '-count=1')
    if ($Race) { $env:CGO_ENABLED = '1'; $goArgs += '-race' }
    $goArgs += './...'
    & go @goArgs
    $exit = $LASTEXITCODE
} finally {
    Remove-Item Env:KINGRES_TEST_DSN -ErrorAction SilentlyContinue
    Invoke-Instance 'stop'
}
exit $exit
