@echo off
chcp 65001 >nul
setlocal enabledelayedexpansion

REM ============================================================
REM  build-linux.bat —— 在 Windows 上把 Go 程序编译为 Linux 可执行文件
REM  用法: build-linux.bat [入口文件] [输出名] [架构]
REM  示例: build-linux.bat main.go app amd64
REM        build-linux.bat cmd\server\main.go server arm64
REM ============================================================

set "MAINFILE=%~1"
if "%MAINFILE%"=="" set "MAINFILE=cmd\server\main.go"

set "OUTPUT=%~2"
set "ARCH=%~3"
if "%ARCH%"=="" set "ARCH=amd64"

REM 1. 检查入口文件
if not exist "%MAINFILE%" (
    echo [FAIL] 找不到入口文件: %MAINFILE%
    exit /b 1
)

REM 2. 推导输出名
if "%OUTPUT%"=="" (
    for %%F in ("%MAINFILE%") do set "OUTPUT=%%~nF"
)

REM 3. 检查 go
where go >nul 2>nul
if errorlevel 1 (
    echo [FAIL] 未找到 go 命令，请检查 PATH
    exit /b 1
)
for /f "delims=" %%V in ('go version') do echo [OK] %%V

REM 4. 设置交叉编译环境变量（本脚本内生效，不污染系统）
set "GOOS=linux"
set "GOARCH=%ARCH%"
set "CGO_ENABLED=0"

echo.
echo ==^> 目标平台: %GOOS%/%GOARCH%
echo ==^> 执行: go build -trimpath -ldflags "-s -w" -o %OUTPUT% %MAINFILE%
echo.

go build -trimpath -ldflags "-s -w" -o "%OUTPUT%" "%MAINFILE%"
if errorlevel 1 (
    echo.
    echo [FAIL] 编译失败
    endlocal
    exit /b 1
)

echo.
echo [OK] 编译成功
for %%F in ("%OUTPUT%") do (
    echo     输出文件: %%~fF
    echo     文件大小: %%~zF 字节
)
echo.
echo 部署示例：
echo   scp "%OUTPUT%" user@server:/opt/app/
echo   ssh user@server "chmod +x /opt/app/%OUTPUT% ^&^& /opt/app/%OUTPUT%"

endlocal