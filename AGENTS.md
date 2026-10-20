# AGENTS.md

## Project
Go (1.27+) Windows-only tool: `cmd/OneDriveHelper` (LocalSystem service + admin CLI, console subsystem), `cmd/OneDriveHelperAgent` (per-user agent + popups + UAC approval step) and `cmd/OneDriveHelperConfig` (settings window + --import/--export/--validate). The agent and config tool **must be built with `-H=windowsgui`**. Only dependency: `golang.org/x/sys`.

## Commands
Go 1.27+ on PATH (for example `C:\Program Files\Go\bin`; in PowerShell `$env:Path = "C:\Program Files\Go\bin;$env:Path"`). `go.mod` pins the toolchain, which Go downloads if needed.

- Format: `gofmt -w .`
- Vet: `go vet ./...`
- Test: `go test ./...` (`-short` skips the test that builds the service exe for both architectures)
- Regenerate the event message table: `go generate ./cmd/OneDriveHelper`
- Full build (vet + test + amd64/arm64 into `dist\`): `.\build.ps1 -Version x.y.z` (PowerShell)
- Release (build + `release\OneDriveHelper-vX.Y.Z-x64.zip`, `-arm64.zip`, `SHA256SUMS.txt`): `.\release.ps1 -Version x.y.z`
- CI: `.github/workflows/test.yml` (gofmt, vet, test, arm64 build) on every push and pull request.
- Safe live checks on a dev box (no admin, no changes): `dist\amd64\OneDriveHelper.exe check`, `... remediate --dry-run`, `dist\amd64\OneDriveHelperAgent.exe test-popup`, `OneDriveHelperAgent.exe agent --standalone`

## Conventions / gotchas
- Never treat `HKCU\Software\SyncEngines\Providers\OneDrive` as proof a library is active (stale entries persist); active = Accounts\*\Tenants, ScopeIdToMountPointPathCache, UserFolder, HKLM SyncRootManager.
- `libraryScope` lines in `%LOCALAPPDATA%\Microsoft\OneDrive\settings\BusinessN\*.ini` are UTF-16LE without BOM; field 8/9/10 = SiteID/WebID/ListID.
- Remediation must never follow junctions/symlinks; use `internal/fsutil.ReadDir` and the handle-holding walker in `internal/remediate/tree.go`.
- Alerts are custom Win32 popups (`internal/popup`), NOT Windows toasts (user requirement). All window work happens on the popup manager's locked UI thread; other goroutines use `Manager.Show/Hide`. Button actions run on new goroutines.
- Settings: add new ones only as fields on `config.Config` with `reg`, `def`, `label`, `help` (and `min`/`max` for ints) tags. The registry, JSON import/export, validation and the config tool window are all driven from those tags. Place new fields in the `layout` in `internal/configui/form.go`.
- The service image path must stay explicitly quoted (`service.QuotePath`). `%ProgramData%\OneDriveHelper` must stay admin-owned (`logx.PrepareProgramDataDir`).
- Remediation: the UAC-elevated step (`remediate-approve`) only approves; file work runs as the user because it writes into the user's profile (`{Profile}\ODHelper\Saved\<date-time>\<Library>`). Never move file operations into the elevated process.
- Fix sequence (`internal/remediate/execute.go`): move the folder by handle to `{Profile}\ODHelper\Fixing\<run-id>\<SaveAs>` (always the profile volume, outside every scanned folder), back up from there recording each entry from its handle, then delete only verified, unchanged entries through handles that exclude writers. Anything unproven: delete nothing, move back (or keep, state `kept`). Never delete a file that is not in the run's verified record.
- `{Profile}\ODHelper\Fixing\<run-id>\manifest.json` is the intent journal: write each folder's state (`moving`, `staged`, `copied`, `deleting`, ...) before the step it names, via `writeFileAtomic`. `run.lock` there is held open (no sharing) for the whole run; recovery only acts on runs whose lock it can open exclusively.
- Recovery (`remediate.Recover`) runs only in the user's agent (at start and before planning). It never runs elevated or in a dry run; `BuildPlan`, `remediate --dry-run` and `remediate-approve` only list interrupted runs (`ListInterrupted`).
- Cross-process names live in `internal/ipc` (agent mutex, stop event, fix lock). `internal/service` must not import `internal/agent`, `internal/popup` or `internal/remediate` (enforced by a test).
- The fix lock `Global\OneDriveHelperFixing` wraps `Execute` only (not the dialogs or the UAC wait). The installer waits up to 30 minutes while it exists. `Execute` takes a stop func and checks it before starting and before each folder's move.
- OneDrive's process state is three-valued (`onedrive.OneDriveState`: running / not running / unknown). A failed process listing is unknown: never restart, alert or move the outage clock on it.
- Wrap every button, menu and tray action in `Agent.safe` (logs the panic with its stack); goroutines started by `internal/popup` use `safeRun`. A panic in the fix runs recovery straight away.
- Elevation for the fix and the settings tool goes through `winapi.RunElevatedWithDeadline` (15 minutes); the elevated side gets `--deadline` and refuses to act after it.
- Upgrades stage `*.new`, swap with unique `*.old-<timestamp>` names and roll back on any failure (`internal/service/upgrade.go`).
- Event IDs live only in the `// Event IDs.` const block in `internal/elog/elog.go`. After adding one, run `go generate ./cmd/OneDriveHelper` (writes `cmd/OneDriveHelper/eventmsgs_windows_{amd64,arm64}.syso`, the message table the event source points at; commit them). `tools/eventmsgs` tests fail if they're stale. The installer re-registers the source on every install (`elog.Install(svcExe)`), never `InstallAsEventCreate`.
- `onedrive.Gather` records sources it could not read in `Inputs.ReadErrors`; with any read error, `IsActive` is true for every path. A missing key is empty, not an error.
