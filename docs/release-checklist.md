# Release checklist

Run these on a test PC (a Windows 11 VM with snapshots is ideal) before tagging a release. The automated tests (`go test ./...`, run by `build.ps1` and CI) cover the logic, including a fix process that is killed after the backup copy (`TestKilledFixRecoversAndRerunIsSafe`). These checks cover what only a real PC shows: reboots, real programs holding files, a real full disk.

Set up a test user signed in to OneDrive for Business, with a library that has stopped syncing. The quickest way: sync a SharePoint library, then remove its sync relationship in OneDrive's settings, so the folder stays on disk. Put a few files in it, including one only stored locally.

## 1. Build and install

- [ ] `.\release.ps1 -Version x.y.z` passes (vet, tests, both architectures).
- [ ] Install over the previous release with a settings file and a `logo.png` next to it: `OneDriveHelper.exe install --config settings.json`. The upgrade completes and `version` shows the new version.
- [ ] `%ProgramData%\OneDriveHelper\logo.png` exists. Every card (alert, status window, dialogs) shows the logo above the title.
- [ ] Event Viewer: OneDriveHelper events show only their own text, with no "The description for Event ID ... cannot be found".

## 2. Cards instead of message boxes

- [ ] **Fix (admin)**: the confirmation, the administrator approval (after UAC), the progress card and the result are all cards in the bottom-right corner, not Windows message boxes.
- [ ] A long result scrolls with the mouse wheel; the buttons stay visible.
- [ ] **Copy text** on a warning or error copies it and changes to "Copied ✓".
- [ ] Settings tool: **Reset to defaults**, closing with unsaved changes, and an invalid setting all show cards, and the settings window keeps redrawing while a card is open.
- [ ] Settings tool: **Choose...** a logo, **Preview alerts** shows it, **Export** writes `logo.png` next to the JSON file, **Save to this PC** (with UAC) copies it to `%ProgramData%\OneDriveHelper\logo.png`, **Clear** + Save removes it.

## 3. Interrupted fix, with a Windows restart

This checks that a fix stopped half-way leaves a clear, recoverable state.

- [ ] Start **Fix (admin)** on a folder with a few hundred MB of files, approve, and while the progress card is showing, kill `OneDriveHelperAgent.exe` in Task Manager (or pull the VM's power).
- [ ] Restart Windows and sign in as the user.
- [ ] The folder is back in its original place with every file (or, if it couldn't be put back, the status window lists it as unfinished and event 5007 says where it is).
- [ ] `ODHelper\Saved\<date and time>\RECOVERY-REPORT.txt` exists. It names the original path, where the folder is now, the backup path, and which copy has the user's latest edits.
- [ ] Run **Fix (admin)** again. It does not list anything under `ODHelper` as a broken folder. It creates a new `Saved\<date and time>` folder, and the first run's backup and report are unchanged.

## 4. A file changing during the copy

- [ ] Put a large file (1 GB or more) and a `grow.log` in the broken folder. In PowerShell, open `grow.log` so the folder can still be moved, and keep writing to it:

  ```powershell
  $fs = [IO.File]::Open("$path\grow.log", 'Append', 'Write', 'ReadWrite, Delete')
  while ($true) { $fs.WriteByte(120); $fs.Flush(); Start-Sleep -Milliseconds 100 }
  ```

  Start **Fix (admin)** and approve while it runs.
- [ ] Nothing is deleted; the folder is put back; the result says the backup is incomplete (or that a file changed); event 5003 is logged; a recovery report is saved.

## 5. The backup disk running out of space

- [ ] Create a small VHD (for example 200 MB), mount it as `R:`, and set `BackupRoot` to `R:\ODHelper`, with `FreeSpaceMarginMB` at `0`.
- [ ] With a folder smaller than the free space, start a fix and, while it copies, fill `R:` from another window (for example `fsutil file createnew R:\fill.bin <bytes>`).
- [ ] Nothing is deleted; the folder is put back; the message says the files could not all be saved; a recovery report is saved.
- [ ] With a folder bigger than the free space, the fix refuses before changing anything ("not enough free disk space").

## 6. Locked files: restart or sign out to finish

On a single-user workstation:

- [ ] Open a file from the broken folder in Word or Excel and leave it open. Run **Fix (admin)** and approve.
- [ ] The folder is skipped, and the card offers **Not now**, **Restart later** and **Restart now**. Event 5014 is logged when you pick a restart; `%USERPROFILE%\ODHelper\pending-fix.json` appears.
- [ ] **Restart now**: Windows asks programs to close (Word asks to save). After signing in again, the fix finishes on its own without a UAC prompt (event 5015), and the result card appears. `pending-fix.json` is gone.
- [ ] **Restart later**, then just close and reopen the agent (Task Manager) without signing out: nothing happens. Sign out and in: the fix finishes.

On a Remote Desktop session host (and, if you have them, Windows multi-session / Citrix / FSLogix):

- [ ] The card offers **Sign out now**, **Sign out later** and **Not now** instead of a restart.
- [ ] **Sign out now** signs only that user out; other users' sessions carry on. At the user's next sign-in the fix finishes on its own (event 5015).
- [ ] In a farm with FSLogix profiles: pick **Sign out now** on host A, sign in again so you land on host B. The fix finishes on host B, and event 5015 there says it was approved on host A.
- [ ] With a second user signed in on a workstation (fast user switching), a sign-out is offered, not a restart.

Both:

- [ ] With `RestartToFinish` set to `0`, neither is offered; the user is told to close the program and try again.
- [ ] A `pending-fix.json` older than 3 days is removed at sign-in without fixing anything.

A dropped library that Windows still has registered as a sync folder, from a library the user can only read. This was seen on a real RDS host as `ACME-Training - Documents (1)`: moving it returned "Access to the cloud file is denied", even for an administrator, and its copies carried `Everyone:(DENY)(D,WD,AD,DC)`:

- [ ] Before the fix, `fsutil reparsepoint query "<folder>"` shows a cloud tag (`0x9000xxxx1a`) and `OneDriveHelper.exe check` lists the folder as orphaned.
- [ ] The fix logs event 5017, moves the folder, removes the read-only lock from the copies, backs them up and removes the folder. No UAC prompt is needed beyond the normal approval.
- [ ] No restart or sign-out is offered for it (it is not a locked folder).
- [ ] The live library next to it (same name without "(1)") is untouched and still syncing, and its files still show `Everyone:(DENY)` in `icacls`.

## 7. Uninstall

- [ ] `uninstall --purge` removes the service, `HKLM\SOFTWARE\OneDriveHelper` and `%ProgramData%\OneDriveHelper\logo.png`.
