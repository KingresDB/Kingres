# SPDX-License-Identifier: AGPL-3.0-only
#
# The loops of the definition of done of the failover part (docs/design/ha-phase5-part2.md, section 9: the tests marked (loop) are run 30 times, and the
# measured times of section 2.5 are reported). Each group of tests runs through run-integration.ps1 (which holds the mutex of the cluster tests) on the
# PostgreSQL major of -Install; the output of every run is kept in -OutDir, and the lines "the failover took ..." and "the writes were stopped for ..." that the
# tests log are collected in times.txt.
#
# The tests run round by round: a round is one invocation of run-integration.ps1 (one build of the test binary) for the tests of every selected group, with -Count 1, so a
# failure names the round it happened in; no round begins after -MaxMinutes (a round that has begun is finished). With -PerGroup each group is its own invocation instead
# (a build for each, which a run of a few tests pays for). The duration of every test is read from the "--- PASS" and "--- FAIL" lines of the logs, and a summary per test
# (runs, failures, shortest, mean and longest in seconds) is written to summary.txt and to the end of times.txt.
#
# Usage (from kingres\control-plane):
#   .\scripts\run-failover-loops.ps1 [-Count 30] [-MaxMinutes 120] [-Install ~/kw-cp19/install] [-OutDir .\loops-out] [-Lane l9] [-Only name] [-PerGroup]
param(
    # The number of rounds (runs of each group).
    [int]$Count = 30,
    [string]$Install = '',
    [string]$OutDir = (Join-Path $PSScriptRoot '..\loops-out'),
    # Run one group only (by name below).
    [string]$Only = '',
    # No round begins after this many minutes since the start (0: no bound).
    [int]$MaxMinutes = 0,
    # The lane of run-integration.ps1.
    [string]$Lane = '',
    # One invocation per group, not per round.
    [switch]$PerGroup
)
$ErrorActionPreference = 'Stop'
$groups = [ordered]@{
    # T1 and T4: a server dies under load; the leader ends at every step boundary.
    'kill-under-load'  = 'TestFailoverOfAGroupWhoseServerIsDead|TestFailoverSurvivesTheLeaderEndingAtAStepBoundary'
    # T11, T11b, T11c: the old primary comes back, never comes back, runs unfenced.
    'old-primary'      = 'TestOldPrimaryReturns|TestOldPrimaryReturnsAndIsRebuiltByABaseBackup|TestOldPrimaryNeverReturns|TestOldPrimaryRunningUnfenced'
    # T12, T12b, T12c: the planned switchover, and the way back.
    'switchover'       = 'TestSwitchoverOfAGroup|TestSwitchoverVerificationFails|TestSwitchoverWithAnUncleanStop|TestSwitchoverCancelBeforeAndAfterThePointOfNoReturn|TestSwitchoverSurvivesTheLeaderEndingWhileTheWritesAreStopped|TestSwitchoverSurvivesACleanStopThatOutlastsTheLease'
    'restore'          = 'TestSwitchoverAndBack|TestSwitchoverStatusNamesThePhaseAndThePause'
    # T6: the map group.
    'map-group'        = 'TestMapGroupPrimaryLost|TestMapGroupBootstrapSurvivesKingresdEnding|TestMapGroupEngineGateStaysClosedUntilTheBootstrapEnds'
    # The map group with a live old leader: the planned switchover and the hard failover.
    'map-switchover'   = 'TestSwitchoverOfTheMapGroup$|TestSwitchoverOfTheMapGroupWhoseProofIsRefused|TestSwitchoverOfTheMapGroupWhoseOldLeaderEndsAtTheStop'
    'map-hard'         = 'TestHardFailoverOfTheMapGroup'
    # T13: the automatic failover.
    'automatic'        = 'TestAutomaticFailoverRulesAndTheCounter|TestAutomaticFailoverOfTheMapGroup'
}
New-Item -ItemType Directory -Force $OutDir | Out-Null
$times = Join-Path $OutDir 'times.txt'
$started = Get-Date
"loops started $(Get-Date -Format s), rounds $Count, bound $MaxMinutes min, install '$Install'" | Set-Content $times
$names = @($groups.Keys | Where-Object { -not $Only -or $_ -eq $Only })
$stats = @{}
$failed = @()
$stop = $false
function Invoke-Loop([string]$label, [string]$regex) {
    $log = Join-Path $OutDir "$label.log"
    $a = @{ Packages = @('./internal/clustertest'); Run = $regex; Count = 1 }
    if ($Install) { $a.Install = $Install }
    if ($Lane) { $a.Lane = $Lane }
    $t0 = Get-Date
    & (Join-Path $PSScriptRoot 'run-integration.ps1') @a *> $log
    $code = $LASTEXITCODE
    "$label exit $code after $([int]((Get-Date) - $t0).TotalSeconds) s" | Add-Content $times
    Select-String -Path $log -Pattern 'the failover took|the writes were stopped for|--- FAIL' | ForEach-Object { $_.Line.Trim() } | Add-Content $times
    foreach ($m in (Select-String -Path $log -Pattern '^--- (PASS|FAIL): (\S+) \(([0-9.]+)s\)')) {
        $g = $m.Matches[0].Groups
        if (-not $stats.ContainsKey($g[2].Value)) { $stats[$g[2].Value] = [pscustomobject]@{ Runs = 0; Failed = 0; Seconds = @() } }
        $s = $stats[$g[2].Value]
        $s.Runs++; $s.Seconds += [double]$g[3].Value
        if ($g[1].Value -eq 'FAIL') { $s.Failed++ }
    }
    if ($code -ne 0) { $script:failed += $label }
}
for ($round = 1; $round -le $Count -and -not $stop; $round++) {
    if ($MaxMinutes -gt 0 -and ((Get-Date) - $started).TotalMinutes -ge $MaxMinutes) { break }
    if ($PerGroup) {
        foreach ($name in $names) {
            if ($MaxMinutes -gt 0 -and ((Get-Date) - $started).TotalMinutes -ge $MaxMinutes) { $stop = $true; break }
            Invoke-Loop "$name-$round" $groups[$name]
        }
    } else {
        Invoke-Loop "round-$round" (($names | ForEach-Object { $groups[$_] }) -join '|')
    }
}
$summary = Join-Path $OutDir 'summary.txt'
"loops ended $(Get-Date -Format s) after $([int]((Get-Date) - $started).TotalMinutes) min" | Set-Content $summary
& {
    foreach ($n in ($stats.Keys | Sort-Object)) {
    $s = $stats[$n]
    $m = $s.Seconds | Measure-Object -Minimum -Maximum -Average
    "{0} : {1} runs, {2} failed, {3} s shortest, {4} s mean, {5} s longest" -f $n, $s.Runs, $s.Failed, $m.Minimum, [int]$m.Average, $m.Maximum
    }
} | Tee-Object -Append -FilePath $summary | Add-Content $times
if ($failed.Count -gt 0) { Write-Error "failed runs: $($failed -join ', ')"; exit 1 }
