# Security

## Reporting a problem

If you think you've found a security issue, please don't open a public issue. Use GitHub's **Report a vulnerability** button on the Security tab of this repository instead, and we'll get back to you.

## How OneDrive Helper is put together, security-wise

* **No network listener, no web requests, no telemetry.** The app never talks to the internet. The only "outside" input is the Windows registry, the file system and its own settings.
* **The service** runs as LocalSystem and does one job: starting the per-user part in each session. It only ever starts `OneDriveHelperAgent.exe` from its own folder in `C:\Program Files\OneDriveHelper`, which standard users can't write to. Its image path is always quoted.
* **Everything that touches the user's files runs as the user.** That includes reading OneDrive's settings, restarting OneDrive, copying and deleting folders, and opening links. The UAC prompt in the fix is an approval step to prevent mistakes. It isn't used to do the file work, because doing that elevated inside a folder the user controls would let them redirect an administrator's writes.
* **Folders are never followed through junctions or symbolic links.** Every part of a folder's path below the profile is checked, and a folder behind a link is refused. The fix only touches folders inside the user's own profile, never a folder containing their Desktop or Documents, and never one OneDrive is actually syncing (checked before the folder is moved and again before anything is deleted). If OneDrive's settings can't be read, nothing is fixed.
* **The fix never deletes a file it hasn't proven it saved.** Each folder is moved aside by handle, each file is recorded while it is copied and flushed to disk, and only recorded, unchanged files are deleted, through handles that lock out other writers. Every step is written to a journal before it happens, so an interrupted fix is put back or reported; recovery runs only as the user.
* **The service's log folder** (`%ProgramData%\OneDriveHelper`) is created with its final permissions (owned by Administrators, writable only by SYSTEM and Administrators), so it is never briefly user-writable. A folder planted there in advance by a user is moved aside rather than trusted.
* **Approvals expire.** An administrator approval that comes more than 15 minutes after the request does nothing.
* **Settings** live in `HKLM`, so only administrators can change them. Every settings file is validated before it's applied.
* **Values read from the user's registry are treated as untrusted.** For example, only `https://` site addresses are ever opened, and only a file actually called `OneDrive.exe` is ever started.
* **System DLLs only.** All Windows DLLs are loaded from System32, so DLL planting next to the programs doesn't work.
* **Only standard Go and `golang.org/x/sys`.** There are no other third-party dependencies.

## Deploying safely

* Verify downloads against `SHA256SUMS.txt`, or use the RMM script in the README, which checks the hash for you. Better still, host the zip yourself.
* The release binaries aren't code-signed. If you deploy widely, consider building from source and signing with your own certificate.
* When scripting as SYSTEM, don't use fixed file names in `C:\Windows\Temp`, because ordinary users can create files there. The scripts in the README use a randomly named folder locked to SYSTEM and Administrators.
