; ALL SHARE — Windows installer
; Powered by MMC
;
; Built with Inno Setup 6. Inno was chosen over WiX and NSIS deliberately:
;
;   WiX produces an MSI, which is the right answer for fleet deployment through
;   group policy, but its learning curve and build chain buy nothing for a
;   product one person installs on their own PC.
;
;   NSIS produces smaller installers but requires micromanaging every step,
;   including service installation and uninstall, which is exactly where an
;   installer bug becomes a machine that cannot be cleaned up.
;
;   Inno handles elevation, uninstall, upgrade-in-place and Start Menu entries
;   itself, and its Pascal scripting is enough for the two things this installer
;   genuinely has to get right: registering the service, and never leaving it
;   running after an uninstall.
;
; Build:  iscc installer\allshare.iss
; Output: dist\AllShareSetup.exe

#define AppName "ALL SHARE"
#define AppPublisher "MMC"
#define AppVersion GetEnv('ALLSHARE_VERSION')
#if AppVersion == ""
  #define AppVersion "1.0.0"
#endif
#define AgentExe "allshare-agent.exe"
#define ServiceName "ALLShare"

[Setup]
AppId={{7B2E9C41-5F3A-4D18-9A6E-2C7D4B8E1F05}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
DisableDirPage=auto
OutputDir=..\dist
OutputBaseFilename=AllShareSetup
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; Screen capture and the service both need administrator rights, and asking
; once at the start is far better than failing halfway through.
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64compatible
ArchitecturesAllowed=x64compatible
MinVersion=10.0.17763
UninstallDisplayName={#AppName}
UninstallDisplayIcon={app}\{#AgentExe}
CloseApplications=yes
RestartApplications=no
SetupLogging=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Messages]
WelcomeLabel2=This will install [name/ver] on your computer.%n%nALL SHARE lets you use this PC from your other devices. After installing, you will get a code to type on the device you want to connect from.

[Tasks]
Name: "desktopicon"; Description: "Create a shortcut on the desktop"; GroupDescription: "Shortcuts:"
Name: "firewall"; Description: "Allow ALL SHARE through Windows Firewall"; GroupDescription: "Network:"; Flags: checkedonce

[Files]
Source: "..\dist\windows\{#AgentExe}"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\docs\user-guide.md"; DestDir: "{app}\docs"; Flags: ignoreversion isreadme
Source: "..\dist\allshare-client.zip"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist

[Icons]
Name: "{group}\{#AppName}"; Filename: "{app}\{#AgentExe}"; Parameters: "pair"; Comment: "Add a device to this PC"
Name: "{group}\{#AppName} status"; Filename: "{app}\{#AgentExe}"; Parameters: "status"; Comment: "See what ALL SHARE is doing"
Name: "{group}\Uninstall {#AppName}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AgentExe}"; Parameters: "pair"; Tasks: desktopicon

[Run]
; The service address is collected during setup, so the user never edits a
; configuration file by hand.
Filename: "{app}\{#AgentExe}"; \
  Parameters: "install -service ""{code:GetServiceAddress}"" -name ""{code:GetDeviceName}"""; \
  StatusMsg: "Setting up ALL SHARE..."; Flags: runhidden waituntilterminated

Filename: "{app}\{#AgentExe}"; Description: "Add a device now"; \
  Parameters: "pair"; Flags: postinstall nowait skipifsilent

[UninstallRun]
Filename: "{app}\{#AgentExe}"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[Code]
var
  ConfigPage: TInputQueryWizardPage;

procedure InitializeWizard;
begin
  ConfigPage := CreateInputQueryPage(wpSelectTasks,
    'Set up ALL SHARE',
    'Where should this PC reach ALL SHARE?',
    'ALL SHARE connects your devices through a small service that you run or that ' +
    'someone has set up for you. Enter its address below.' + #13#10#13#10 +
    'If you are not sure, leave it as it is — you can change it later from the ' +
    'ALL SHARE Start Menu entry.');

  ConfigPage.Add('ALL SHARE service address:', False);
  ConfigPage.Add('Name to show for this PC:', False);

  ConfigPage.Values[0] := 'wss://';
  ConfigPage.Values[1] := GetComputerNameString();
end;

function GetServiceAddress(Param: String): String;
begin
  Result := Trim(ConfigPage.Values[0]);
  if (Result = 'wss://') or (Result = '') then
    Result := '';
end;

function GetDeviceName(Param: String): String;
begin
  Result := Trim(ConfigPage.Values[1]);
  if Result = '' then
    Result := GetComputerNameString();
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Address: String;
begin
  Result := True;
  if CurPageID <> ConfigPage.ID then
    Exit;

  Address := Trim(ConfigPage.Values[0]);
  if (Address = '') or (Address = 'wss://') then
  begin
    // An empty address is allowed: the user may be setting the PC up before the
    // service exists. Say so plainly rather than blocking them.
    Result := (MsgBox('No service address was entered.' + #13#10#13#10 +
      'ALL SHARE will install, but this PC will not be reachable until you set ' +
      'an address. You can do that later from the Start Menu.' + #13#10#13#10 +
      'Continue anyway?', mbConfirmation, MB_YESNO) = IDYES);
    Exit;
  end;

  if (Pos('wss://', LowerCase(Address)) <> 1) and (Pos('ws://', LowerCase(Address)) <> 1) then
  begin
    MsgBox('That address does not look right.' + #13#10#13#10 +
      'It should start with wss:// — for example:' + #13#10 +
      '    wss://allshare.example.com/rv',
      mbError, MB_OK);
    Result := False;
  end;
end;

// Windows Firewall
//
// ALL SHARE listens on no fixed port: it makes outbound connections and receives
// peer-to-peer traffic on ports the operating system chooses. A program rule is
// therefore both sufficient and much narrower than opening a port range.
procedure AddFirewallRule();
var
  ResultCode: Integer;
  ExePath: String;
begin
  ExePath := ExpandConstant('{app}\{#AgentExe}');
  Exec('netsh', 'advfirewall firewall delete rule name="ALL SHARE"',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Exec('netsh', 'advfirewall firewall add rule name="ALL SHARE" dir=in action=allow ' +
    'program="' + ExePath + '" enable=yes profile=private,domain',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

procedure RemoveFirewallRule();
var
  ResultCode: Integer;
begin
  Exec('netsh', 'advfirewall firewall delete rule name="ALL SHARE"',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and WizardIsTaskSelected('firewall') then
    AddFirewallRule();
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    RemoveFirewallRule();
end;

// An in-place upgrade must stop the running service first, or Windows will hold
// the executable open and the install will fail with a file-in-use error.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
begin
  Result := '';
  if not RegKeyExists(HKLM, 'SYSTEM\CurrentControlSet\Services\{#ServiceName}') then
    Exit;
  Exec('net', 'stop {#ServiceName}', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Sleep(1500);
end;
