package main

import (
	"bytes"
	"shell"
	"shell/config"
)

func runGCCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.GCCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = shell.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func runTDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.TDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = shell.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func runHDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.HDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = shell.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func updatePaths(pid int, gcPath, tdPath, hdPath *string) {
	var path []byte
	path, _ = runGCCaptureCmd(pid)
	if len(path) > 0 {
		*gcPath = string(path)
	}
	path, _ = runTDCaptureCmd(pid)
	if len(path) > 0 {
		*tdPath = string(path)
	}
	path, _ = runHDCaptureCmd(pid)
	if len(path) > 0 {
		*hdPath = string(path)
	}
}
