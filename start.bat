@echo off
title workbuddy2api
cd /d "%~dp0"

echo ============================================================
echo   workbuddy2api - OpenAI compatible gateway
echo   press Ctrl+C to stop
echo ============================================================
echo.

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1"

echo.
echo [workbuddy2api] stopped. exit code = %errorlevel%
echo.
pause
