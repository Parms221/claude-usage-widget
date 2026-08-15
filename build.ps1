# Compila el widget como aplicación de ventana (sin consola).
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# -unsafeptr=false: los callbacks Win32 (CBT hook / wndproc) convierten
# uintptr->unsafe.Pointer sobre punteros provistos por el SO; es el patrón
# estándar y vet lo marca como falso positivo.
go vet -unsafeptr=false ./...
if (-not $?) { throw "go vet falló" }

go build -ldflags "-H windowsgui -s -w" -o ClaudeUsageWidget.exe .
if (-not $?) { throw "go build falló" }

Write-Host "OK -> $PSScriptRoot\ClaudeUsageWidget.exe"
