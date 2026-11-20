# SPDX-License-Identifier: AGPL-3.0-only
#
# The loops of the definition of done of the failover part (docs/design/ha-phase5-part2.md, section 9: the tests marked (loop) are run 30 times, and the
# measured times of section 2.5 are reported). Each group of tests runs through run-integration.ps1 (which holds the mutex of the cluster tests) with
# -Count N on the PostgreSQL major of -Install; the output of every group is kept in -OutDir, and the lines "the failover took ..." and "the writes were
# stopped for ..." that the tests log are collected in times.txt.
#
# Usage (from kingres\control-plane):
#   .\scripts\run-failover-loops.ps1 [-Count 30] [-Install ~/kw-cp19/install] [-OutDir .\loops-out] [-Only name]
param(
    [int]$Count = 30,
    [string]$Install = '',
    [string]$OutDir = (Join-Path $PSScriptRoot '..\loops-out'),
    # Run one group only (by name below).
    [string]$Only = ''
)
$ErrorActionPreference = 'Stop'
$groups = [ordered]@{
    # T1 and T4: a server dies under load; the leader ends at every step boundary.
    'kill-under-load'  = 'TestFailoverOfAGroupWhoseServerIsDead|TestFailoverSurvivesTheLeaderEndingAtAStepBoundary'
    # T11, T11b, T11c: the old primary comes back, never comes back, runs unfenced.
    'old-primary'      = 'TestOldPrimaryReturns|TestOldPrimaryReturnsAndIsRebuiltByABaseBackup|TestOldPrimaryNeverReturns|TestOldPrimaryRunningUnfenced'
    # T12, T12b, T12c: the planned switchover, and the way back.
    'switchover'       = 'TestSwitchoverOfAGroup|TestSwitchoverVerificationFails|TestSwitchoverWithAnUncleanStop|TestSwitchoverCancelBeforeAndAfterThePointOfNoReturn|TestSwitchoverSurvivesTheLeaderEndingWhileTheWritesAreStopped'
    'restore'          = 'TestSwitchoverAndBack|TestSwitchoverStatusNamesThePhaseAndThePause'
    # T6: the map group.
    'map-group'        = 'TestMapGroupPrimaryLost|TestMapGroupBootstrapSurvivesKingresdEnding|TestMapGroupEngineGateStaysClosedUntilTheBootstrapEnds'
    # T13: the automatic failover.
    'automatic'        = 'TestAutomaticFailoverRulesAndTheCounter|TestAutomaticFailoverOfTheMapGroup'
}
New-Item -ItemType Directory -Force $OutDir | Out-Null
$times = Join-Path $OutDir 'times.txt'
"loops started $(Get-Date -Format s), count $Count, install '$Install'" | Set-Content $times
$failed = @()
foreach ($name in $groups.Keys) {
    if ($Only -and $Only -ne $name) { continue }
    $log = Join-Path $OutDir "$name.log"
    $a = @{ Packages = @('./internal/clustertest'); Run = $groups[$name]; Count = $Count }
    if ($Install) { $a.Install = $Install }
    $start = Get-Date
    & (Join-Path $PSScriptRoot 'run-integration.ps1') @a *> $log
    $code = $LASTEXITCODE
    "$name exit $code after $([int]((Get-Date) - $start).TotalMinutes) min" | Add-Content $times
    Select-String -Path $log -Pattern 'the failover took|the writes were stopped for|--- FAIL' | ForEach-Object { $_.Line.Trim() } | Add-Content $times
    if ($code -ne 0) { $failed += $name }
}
if ($failed.Count -gt 0) { Write-Error "failed groups: $($failed -join ', ')"; exit 1 }
