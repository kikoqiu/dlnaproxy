@echo off
SET GOARCH=amd64
SET GOOS=linux
ECHO Compile Linux
go build -o build\dlnaproxy
SET GOOS=windows
ECHO Compile Windows
go build -o build\dlnaproxy.exe

SET GOARCH=arm
SET GOOS=linux
ECHO Compile Linux ARM
go build -o build\dlnaproxy_armhf
