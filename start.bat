@echo off
chcp 65001 >nul
setlocal
set "CODEX_ARGS=-c sandbox_mode=workspace-write"
rem AI Agent Room launcher
rem   Usage: start.bat [workdir]
rem   - Double-click: start in the previous workdir (this folder on first run)
rem   - Drag and drop a folder onto this batch: start with that folder as the workdir
rem   - Extra options go in the AI_AGENT_ROOM_OPTS environment variable (e.g. set AI_AGENT_ROOM_OPTS=-port 8788 -max-hops 20)
rem   Keep this file ASCII only: cmd with chcp 65001 can split lines that contain multibyte characters.

cd /d "%~dp0"

rem Without a workdir argument, start in the previous workdir (or this folder)
set "WDOPT="
if not "%~1"=="" set WDOPT=-workdir "%~1"

rem If Go is available, build every time so the latest source is used (cached builds finish in seconds)
where go >nul 2>nul
if errorlevel 1 goto run
if not exist ai-agent-room.exe goto build
rem ai-agent-room.exe in this folder cannot be overwritten while running. ai-agent-room.exe in other folders is ignored (and not stopped)
powershell -NoProfile -Command "$p=(Resolve-Path ai-agent-room.exe).Path; if (Get-Process ai-agent-room -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $p }) { exit 1 }" <nul
if not errorlevel 1 goto checked
echo ai-agent-room.exe is running. Exit it first, then run this batch again.
pause
exit /b 1
:checked
rem Show the list and the question on the screen (con), so a human sees them even when output is redirected to a file.
rem Fall back to standard output when con is not available.
set "TTY=>con"
(type nul >con) 2>nul || set "TTY="
rem Show files changed since the last build (src and this batch), so source changes by agents are not applied silently
powershell -NoProfile -Command "$t=(Get-Item ai-agent-room.exe).LastWriteTime; $c=@(Get-ChildItem src -Recurse -File -Include *.go,*.html,*.js,*.css,go.mod,go.sum) + @(Get-Item start.bat) | Where-Object { $_.LastWriteTime -gt $t }; if ($c) { $c | ForEach-Object { '  ' + $_.FullName.Substring($PWD.Path.Length + 1) + '  ' + $_.LastWriteTime.ToString('yyyy-MM-dd HH:mm') }; exit 1 }" <nul %TTY%
if not errorlevel 1 goto build
%TTY% echo The files above changed after the last build. Check that there are no unexpected changes.
rem When key input is not available (e.g. < NUL), choice fails with 255. Keep the list and continue.
%TTY% echo Build and start with these changes? Y to continue, N to cancel.
choice /C YN /M "Build and start?" %TTY%
if errorlevel 255 goto build
if errorlevel 2 (
  %TTY% echo Canceled.
  pause %TTY%
  exit /b 1
)
:build
echo Building...
rem Sources are in src. -C must come first; -o is relative to src
go build -C src -o ..\ai-agent-room.exe .
if not errorlevel 1 goto run
echo Build failed. If ai-agent-room.exe is running, exit it and run this batch again.
pause
exit /b 1

:run

if not exist ai-agent-room.exe (
  echo ai-agent-room.exe not found. Install Go, or place a prebuilt ai-agent-room.exe in this folder.
  pause
  exit /b 1
)

"%~dp0ai-agent-room.exe" %WDOPT% %AI_AGENT_ROOM_OPTS%
if errorlevel 1 pause
