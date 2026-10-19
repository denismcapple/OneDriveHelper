# Security

## Reporting a problem

If you think you've found a security issue, please don't open a public issue. Use GitHub's **Report a vulnerability** button on the Security tab of this repository instead, and we'll get back to you.

## How OneDrive Helper is put together, security-wise

* **No network listener, no web requests, no telemetry.** The app never talks to the internet. The only "outside" input is the Windows registry, the file system and its own settings.
* **The service** runs as LocalSystem and does one job: starting the per-user part in each session. It only ever starts `OneDriveHelperAgent.exe` from its own folder in `C:\Program Files\OneDriveHelper`, which standard users can't write to. Its image path is always quoted.
* **Everything that touches the user's files runs as the user.** That includes reading OneDrive's settings, restarting OneDrive, copying and deleting folders, and opening links. The UAC prompt in the fix is an approval step to prevent mistakes. It isn't used to do the file work, because doing that elevated inside a folder the user controls would let them redirect an administrator's writes.
* **Folders are never followed through junctions or symbolic links.** The fix only touches folders inside the user's own profile, never a folder containing their Desktop or Documents, and never one OneDrive is actually syncing (checked again just before acting).
* **The service's log folder** (`%ProgramData%\OneDriveHelper`) is owned by Administrators and only writable by SYSTEM and Administrators. A folder planted there in advance by a user is moved aside rather than trusted.
* **Settings** live in `HKLM`, so only administrators can change them. Every settings file is validated before it's applied.
* **Values read from the user's registry are treated as untrusted.** For example, only `https://` site addresses are ever opened, and only a file actually called `OneDrive.exe` is ever started.
* **System DLLs only.** All Windows DLLs are loaded from System32, so DLL planting next to the programs doesn't work.
* **Only standard Go and `golang.org/x/sys`.** There are no other third-party dependencies.

## Deploying safely

* Verify downloads against `SHA256SUMS.txt`, or use the RMM script in the README, which checks the hash for you. Better still, host the zip yourself.
* The release binaries aren't code-signed. If you deploy widely, consider building from source and signing with your own certificate.
* When scripting as SYSTEM, don't use fixed file names in `C:\Windows\Temp`, because ordinary users can create files there. The scripts in the README use a randomly named folder locked to SYSTEM and Administrators.
