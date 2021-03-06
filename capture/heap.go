package capture

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"shell"
	"shell/logger"
)

const hdOut = "heap_dump.out"
const hdZip = "heap_dump.zip"

type HeapDump struct {
	Capture
	JavaHome string
	Pid      int
	hdPath   string
	dump     bool
}

func NewHeapDump(javaHome string, pid int, hdPath string, dump bool) *HeapDump {
	return &HeapDump{JavaHome: javaHome, Pid: pid, hdPath: hdPath, dump: dump}
}

func (t *HeapDump) Run() (result Result, err error) {
	var hd *os.File
	if len(t.hdPath) > 0 {
		var hdf *os.File
		hdf, err = os.Open(t.hdPath)
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("failed to open hdPath(%s) err: %s. Trying to open in the Docker container...", t.hdPath, err.Error())
			hdf, err = os.Open(filepath.Join("/proc", strconv.Itoa(t.Pid), "root", t.hdPath))
		}
		if err != nil {
			logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
		} else {
			logger.Log("copying heap dump data %s", t.hdPath)
			defer func() {
				err := hdf.Close()
				if err != nil {
					logger.Log("failed to close hd file %s cause err: %s", t.hdPath, err.Error())
				}
			}()
			hd, err = os.Create(hdOut)
			if err != nil {
				return
			}
			defer func() {
				err := hd.Close()
				if err != nil {
					logger.Log("failed to close hd file %s cause err: %s", hdOut, err.Error())
				}
				err = os.Remove(hdOut)
				if err != nil {
					logger.Log("failed to rm hd file %s cause err: %s", hdOut, err.Error())
				}
			}()
			_, err = io.Copy(hd, hdf)
			if err != nil {
				return
			}
			_, err = hd.Seek(0, 0)
			if err != nil {
				return
			}
			logger.Log("copied heap dump data %s", t.hdPath)
		}
	}
	if t.Pid > 0 && hd == nil && t.dump {
		logger.Log("capturing heap dump data")
		var dir string
		dir, err = os.Getwd()
		if err != nil {
			return
		}
		fp := filepath.Join(dir, fmt.Sprintf("%s.%d.%d", hdOut, t.Pid, time.Now().Unix()))
		err = t.heapDump(fp)
		if err != nil {
			fp = filepath.Join(os.TempDir(), fmt.Sprintf("%s.%d.%d", hdOut, t.Pid, time.Now().Unix()))
			err = t.heapDump(fp)
			if err != nil {
				return
			}
		}
		defer func() {
			err := os.Remove(fp)
			if err != nil {
				logger.Trace().Err(err).Str("file", fp).Msg("failed to rm hd file")
			}
		}()
		hd, err = os.Open(fp)
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("Failed to %s. Trying to open in the Docker container...", err.Error())
			fp = filepath.Join("/proc", strconv.Itoa(t.Pid), "root", fp)
			hd, err = os.Open(fp)
		}
		if err != nil {
			err = fmt.Errorf("failed to open heap dump file: %w", err)
			return
		}
		defer func() {
			err := hd.Close()
			if err != nil {
				logger.Log("failed to close hd file %s cause err: %s", fp, err.Error())
			}
		}()
		logger.Log("captured heap dump data")
	}
	if hd == nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		result.Msg = "skipped heap dump"
		return
	}
	zipfile, err := os.Create(hdZip)
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	defer func() {
		if err := zipfile.Close(); err != nil {
			logger.Log("failed to close zip file: %s", err.Error())
		}
	}()
	writer := zip.NewWriter(bufio.NewWriter(zipfile))
	out, err := writer.Create(hdOut)
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	_, err = io.Copy(out, hd)
	if err != nil {
		err = fmt.Errorf("failed to zip heap dump file: %w", err)
		return
	}
	err = writer.Close()
	if err != nil {
		err = fmt.Errorf("failed to finish zipping heap dump file: %w", err)
		return
	}
	e := zipfile.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}

	result.Msg, result.Ok = shell.PostData(t.endpoint, "hd&Content-Encoding=zip", zipfile)
	return
}

func (t *HeapDump) heapDump(fp string) (err error) {
	var output []byte
	output, err = shell.CommandCombinedOutput(shell.Command{path.Join(t.JavaHome, "/bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_dump", fp}, shell.SudoHooker{PID: t.Pid})
	logger.Log("Output from jcmd: %s, %v", output, err)
	if err != nil ||
		bytes.Index(output, []byte("No such file")) >= 0 ||
		bytes.Index(output, []byte("Permission denied")) >= 0 {
		if len(output) > 1 {
			err = fmt.Errorf("%w because %s", err, output)
		}
		var e2 error
		output, e2 = shell.CommandCombinedOutput(shell.Command{shell.Executable(), "-p", strconv.Itoa(t.Pid), "-hdPath", fp, "-hdCaptureMode"},
			shell.EnvHooker{"pid": strconv.Itoa(t.Pid)},
			shell.SudoHooker{PID: t.Pid})
		logger.Log("Output from jattach: %s, %v", output, e2)
		if e2 != nil {
			err = fmt.Errorf("%v: %v", e2, err)
			return
		}
		err = nil
	}
	return
}
