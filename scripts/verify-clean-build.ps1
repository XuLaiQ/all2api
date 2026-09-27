[CmdletBinding()]
param(
    [switch]$Docker,
    [switch]$SkipHealth
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$arguments = @("$root/scripts/verify-clean-build.py")
if ($Docker) { $arguments += "--docker" }
if ($SkipHealth) { $arguments += "--skip-health" }

python @arguments
if ($LASTEXITCODE -ne 0) {
    throw "All2API clean build/run verification failed with exit code $LASTEXITCODE"
}
