; AltNet Relay — Inno Setup installer script.
;
; Built by scripts/build-windows.sh; to build by hand, run
;   iscc scripts\AltNetRelay.iss
; after `wails build` has produced app\desktop\build\bin\AltNetRelay.exe.
; Output lands in dist\.
;
; Per-user install, no admin rights. Studio installs machine-wide and
; needs elevation; a messenger has no reason to, and asking for admin to
; install a chat app is exactly the kind of prompt people learn to click
; through. With PrivilegesRequired=lowest, {autopf} resolves to
; %LOCALAPPDATA%\Programs -- the same place per-user installers of other
; desktop apps go.

#define AppName      "AltNet Relay"
#define AppVersion   "1.0.8"
#define AppPublisher "PANMOX"
#define AppExe       "AltNetRelay.exe"

[Setup]
; Stable across every future version, or upgrades and uninstall stop
; recognising earlier installs. Deliberately NOT Studio's AppId: the two
; apps install, upgrade and uninstall independently.
AppId={{0659DA1E-B724-4484-B2CC-B32A44BC2A4B}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL=https://panmox.org
; Shown as "PANMOX" in Windows' list of installed apps and on the installer file.
VersionInfoCompany={#AppPublisher}
VersionInfoProductName={#AppName}
AppCopyright=(c) 2026 PANMOX
PrivilegesRequired=lowest
DefaultDirName={autopf}\AltNet Relay
DefaultGroupName=AltNet Relay
DisableProgramGroupPage=yes
OutputDir=..\dist
OutputBaseFilename=AltNetRelay-Setup-{#AppVersion}
SetupIconFile=..\app\desktop\build\windows\icon.ico
UninstallDisplayIcon={app}\{#AppExe}
UninstallDisplayName={#AppName}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
; Replaces a running copy on upgrade instead of failing on a locked file.
CloseApplications=yes
; Built for 64-bit Windows only, same as Studio.
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "Create a &desktop shortcut"; GroupDescription: "Additional icons:"; Flags: unchecked
; Ticked by default: an always-on messenger only helps if it is running.
Name: "startup"; Description: "&Start AltNet Relay when I sign in to Windows (so messages reach you straight away)"; GroupDescription: "Running:"

[Files]
Source: "..\app\desktop\build\bin\{#AppExe}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\{#AppName}";           Filename: "{app}\{#AppExe}"
Name: "{group}\Uninstall {#AppName}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#AppName}";     Filename: "{app}\{#AppExe}"; Tasks: desktopicon

[Registry]
; Per user, like the install itself. The app starts hidden (--background), in
; the tray, and can turn this off itself under Account.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "AltNetRelay"; ValueData: """{app}\{#AppExe}"" --background"; Flags: uninsdeletevalue; Tasks: startup; Check: not IsAppUpdate
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueName: "AltNetRelay"; Flags: deletevalue; Tasks: not startup; Check: not IsAppUpdate
; An update the app installs itself (silently, /RELAUNCH=...) leaves this
; alone: the person may have changed it in the app since installing.

[Run]
Filename: "{app}\{#AppExe}"; Description: "Launch {#AppName}"; Flags: nowait postinstall skipifsilent
; After an update the app installed itself, reopen it the way it was: in the
; tray (it updated at a quiet moment) or with its window (Restart was pressed).
Filename: "{app}\{#AppExe}"; Parameters: "--background"; Flags: nowait; Check: RelaunchAs('background')
Filename: "{app}\{#AppExe}"; Flags: nowait; Check: RelaunchAs('window')


; ─── What uninstalling does NOT touch, on purpose ───
;
; The user's data lives in %APPDATA%\AltNetRelay: their Relay identity
; keys (relay.key, relay.box), their node key, contacts, and every
; message. Inno only removes files it installed, and the app creates that
; folder itself at runtime, so it survives an uninstall.
;
; That is the whole point, not an oversight. The identity key IS the
; account. Since 1.0.5 a locked copy can usually bring it back (signing in
; again restores it), but not if the person turned that copy off -- then
; deleting the folder would permanently destroy their @handle's keys and
; every conversation, and make them a stranger to everyone they know.
;
; DO NOT add an [UninstallDelete] entry for {userappdata}\AltNetRelay.
; If "remove my data" is ever wanted, it must be an explicit, separately
; confirmed choice inside the app -- never a side effect of uninstalling.

[Code]
function IsAppUpdate: Boolean;
begin
  Result := WizardSilent and (ExpandConstant('{param:relaunch|}') <> '');
end;

function RelaunchAs(Mode: String): Boolean;
begin
  Result := WizardSilent and (CompareText(ExpandConstant('{param:relaunch|}'), Mode) = 0);
end;
