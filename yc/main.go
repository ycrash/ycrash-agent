package main

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	ycattach "shell/ycattach"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pterm/pterm"
	"shell"
	"shell/capture"
	"shell/config"
	"shell/logger"

	"github.com/gentlemanautomaton/cmdline"
)

var wg sync.WaitGroup

func main() {
	err := config.ParseFlags(os.Args)
	if err != nil {
		log.Fatal(err.Error())
	}
	err = logger.Init(config.GlobalConfig.LogFilePath, config.GlobalConfig.LogFileMaxCount,
		config.GlobalConfig.LogFileMaxSize, config.GlobalConfig.LogLevel)
	if err != nil {
		log.Fatal(err.Error())
	}

	if config.GlobalConfig.GCCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureGCLog(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.TDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureThreadDump(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.HDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		if len(config.GlobalConfig.HeapDumpPath) <= 0 {
			logger.Log("-hdPath can not be empty")
			os.Exit(1)
		}
		ret := ycattach.CaptureHeapDump(pid, config.GlobalConfig.HeapDumpPath)
		os.Exit(ret)
	}
	if len(config.GlobalConfig.JCmdCaptureMode) > 0 {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.Capture(pid, "jcmd", config.GlobalConfig.JCmdCaptureMode)
		os.Exit(ret)
	}

	validate()

	osSig := make(chan os.Signal, 1)
	signal.Notify(osSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	go mainLoop()

	select {
	case <-osSig:
		logger.Log("Waiting...")
		wg.Wait()
	}
}

func validate() {
	if len(os.Args) < 2 {
		logger.Log("No arguments are passed.")
		config.ShowUsage()
		os.Exit(1)
	}

	if config.GlobalConfig.ShowVersion {
		logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
		os.Exit(0)
	}

	if !config.GlobalConfig.OnlyCapture {
		if len(config.GlobalConfig.Server) < 1 {
			logger.Log("'-s' yCrash server URL argument not passed.")
			config.ShowUsage()
			os.Exit(1)
		}
		if len(config.GlobalConfig.ApiKey) < 1 {
			logger.Log("'-k' yCrash API Key argument not passed.")
			config.ShowUsage()
			os.Exit(1)
		}
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
		config.ShowUsage()
		os.Exit(1)
	}
	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Log("WARNING: -onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}
	if config.GlobalConfig.AppLogLineCount < 1 {
		logger.Log("%d is not a valid value for 'appLogLineCount' argument. It should be a number larger than 0.", config.GlobalConfig.AppLogLineCount)
		config.ShowUsage()
		os.Exit(1)
	}
}

func startupLogs() {
	logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	msg, ok := shell.StartupAttend()
	logger.Log(
		`startup attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
}

func mainLoop() {
	var once sync.Once
	if config.GlobalConfig.Port > 0 {
		once.Do(startupLogs)
		go func() {
			s, err := shell.NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
			if err != nil {
				logger.Log("WARNING: %s", err)
				return
			}
			s.ProcessPids = processPids
			err = s.Serve()
			if err != nil {
				logger.Log("WARNING: %s", err)
			}
		}()
	}

	if config.GlobalConfig.M3 {
		once.Do(startupLogs)
		go func() {
			for {
				time.Sleep(config.GlobalConfig.M3Frequency)

				timestamp := time.Now().Format("2006-01-02T15-04-05")
				parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
				endpoint := fmt.Sprintf("%s/m3-receiver?%s", config.GlobalConfig.Server, parameters)
				pids, err := process(timestamp, endpoint)
				if err != nil {
					logger.Log("WARNING: process failed, %s", err)
					continue
				}

				if len(pids) > 0 {
					var ps, ns strings.Builder
					i := 0
					for pid, name := range pids {
						ps.WriteString(strconv.Itoa(pid))
						ns.WriteString(name)
						i++
						if i == len(pids) {
							break
						}
						ps.WriteString("-")
						ns.WriteString("-")
					}
					parameters += "&pids=" + ps.String() + "&m3apptoken=" + ns.String()
				}
				finEp := fmt.Sprintf("%s/m3-fin?%s", config.GlobalConfig.Server, parameters)
				resp, err := requestFin(finEp)
				if err != nil {
					logger.Log("WARNING: Request M3 Fin failed, %s", err)
					continue
				}
				if len(resp) <= 0 {
					logger.Log("WARNING: skip empty resp")
					continue
				}
				err = processResp(resp, pids)
				if err != nil {
					logger.Log("WARNING: processResp failed, %s", err)
					continue
				}
			}
		}()
	} else if len(config.GlobalConfig.Pid) > 0 {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			ids, err := shell.GetProcessIds(config.ProcessTokens{config.ProcessToken(config.GlobalConfig.Pid)}, nil)
			if err == nil {
				if len(ids) > 0 {
					for pid := range ids {
						if pid < 1 {
							continue
						}
						fullProcess(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags)
					}
				} else {
					logger.Log("failed to find the target process by unique token %s", config.GlobalConfig.Pid)
				}
			} else {
				logger.Log("unexpected error %s", err)
			}
		} else {
			fullProcess(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags)
		}
		os.Exit(0)
	} else if config.GlobalConfig.Port <= 0 && !config.GlobalConfig.M3 {
		once.Do(startupLogs)
		logger.Log("WARNING: nothing can be done")
		os.Exit(1)
	}
	for {
		msg, ok := shell.Attend()
		logger.Log(
			`daily attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}
}

func processResp(resp []byte, pid2Name map[int]string) (err error) {
	pids, tags, err := shell.ParseJsonResp(resp)
	if err != nil {
		logger.Log("WARNING: Get PID from ParseJsonResp failed, %s", err)
		return
	}
	t := strings.Join(tags, ",")
	one.Lock()
	defer one.Unlock()
	tmp := config.GlobalConfig.Tags
	if len(tmp) > 0 {
		ts := strings.Trim(tmp, ",")
		tmp = strings.Trim(ts+","+t, ",")
	} else {
		tmp = strings.Trim(t, ",")
	}
	_, err = processPidsWithoutLock(pids, pid2Name, config.GlobalConfig.HeapDump, tmp)
	return
}

// only one thread can run capture process
var one sync.Mutex

func processPids(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
	one.Lock()
	defer one.Unlock()

	return processPidsWithoutLock(pids, pid2Name, hd, tags)
}

func processPidsWithoutLock(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
	if len(pids) <= 0 {
		logger.Log("No action needed.")
		return
	}
	set := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if _, ok := set[pid]; ok {
			continue
		}
		set[pid] = struct{}{}
		name := config.GlobalConfig.AppName
		if len(pid2Name) > 0 {
			n, ok := pid2Name[pid]
			if ok {
				name = n
			}
		}
		if len(config.GlobalConfig.CaptureCmd) > 0 {
			_, err := shell.RunCaptureCmd(pid, config.GlobalConfig.CaptureCmd)
			if err != nil {
				logger.Log("WARNING: runCaptureCmd failed %s", err)
				continue
			}
		} else {
			url := fullProcess(pid, name, hd, tags)
			if len(url) > 0 {
				rUrls = append(rUrls, url)
			}
		}
	}
	return
}

func process(timestamp string, endpoint string) (pids map[int]string, err error) {
	one.Lock()
	defer one.Unlock()

	dname := "yc-" + timestamp
	err = os.Mkdir(dname, 0777)
	if err != nil {
		return
	}
	wg.Add(1)
	defer func() {
		defer wg.Done()
		err := os.RemoveAll(dname)
		if err != nil {
			logger.Log("WARNING: Can not remove the current directory: %s", err)
			return
		}
	}()
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	defer os.Chdir(dir)
	err = os.Chdir(dname)
	if err != nil {
		return
	}

	logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	pids, err = shell.GetProcessIds(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	if err == nil && len(pids) > 0 {
		for pid := range pids {
			logger.Log("uploading gc log for pid %d", pid)
			uploadGCLog(endpoint, pid)
		}
	} else if err != nil {
		logger.Log("WARNING: failed to get PID cause %v", err)
	} else {
		logger.Log("WARNING: No PID includes ProcessTokens(%v) without ExcludeTokens(%v)",
			config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	}

	logger.Log("Starting collection of top data...")
	capTop := &capture.Top4M3{}
	top := goCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")
	if top != nil {
		result := <-top
		logger.Log(
			`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}
	return
}

func uploadGCLog(endpoint string, pid int) {
	var gcp string
	bs, err := runGCCaptureCmd(pid)
	dockerID, _ := shell.GetDockerID(pid)
	if err == nil && len(bs) > 0 {
		gcp = string(bs)
	} else {
		output, err := getGCLogFile(pid)
		if err == nil && len(output) > 0 {
			gcp = output
		}
	}
	var gc *os.File
	fn := fmt.Sprintf("gc.%d.log", pid)
	gc, err = processGCLogFile(gcp, fn, dockerID, pid)
	if err != nil {
		logger.Log("process log file failed %s, err: %s", gcp, err.Error())
	}
	var jstat shell.CmdManager
	var triedJAttachGC bool
	if gc == nil || err != nil {
		gc, jstat, err = shell.CommandStartInBackgroundToFile(fn,
			shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"}, shell.SudoHooker{PID: pid})
		if err == nil {
			gcp = fn
			logger.Log("gc log set to %s", gcp)
		} else {
			triedJAttachGC = true
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach...", err.Error())
			gc, jstat, err = captureGC(pid, gc, fn)
			if err == nil {
				gcp = fn
				logger.Log("jattach gc log set to %s", gcp)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
		}
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()
	if jstat != nil {
		err := jstat.Wait()
		if err != nil && !triedJAttachGC {
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach...", err.Error())
			gc, jstat, err = captureGC(pid, gc, fn)
			if err == nil {
				gcp = fn
				logger.Log("jattach gc log set to %s", gcp)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
			err = jstat.Wait()
			if err != nil {
				logger.Log("jattach gc log failed cause %s", err.Error())
			}
		}
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok := shell.PostCustomDataWithPositionFunc(endpoint, fmt.Sprintf("dt=gc&pid=%d", pid), gc, shell.PositionLast5000Lines)
	absGCPath, err := filepath.Abs(gcp)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", gcp, err.Error())
	}
	logger.Log(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absGCPath, ok, msg)
}

func captureGC(pid int, gc *os.File, fn string) (file *os.File, jstat shell.CmdManager, err error) {
	if gc != nil {
		err = gc.Close()
		if err != nil {
			return
		}
		err = os.Remove(fn)
		if err != nil {
			return
		}
	}
	file, jstat, err = shell.CommandStartInBackgroundToFile(fn,
		shell.Command{shell.Executable(), "-p", strconv.Itoa(pid), "-gcCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(pid)}, shell.SudoHooker{PID: pid})
	return
}

func fullProcess(pid int, appName string, hd bool, tags string) (rUrl string) {
	var agentLogFile *os.File
	var err error
	defer func() {
		if err != nil {
			logger.Error().Err(err).Msg("unexpected error")
		}
		if agentLogFile == nil {
			return
		}
		err := logger.StopWritingToFile()
		if err != nil {
			logger.Info().Err(err).Msg("Failed to stop writing to file")
		}
	}()
	// -------------------------------------------------------------------
	//  Create output files
	// -------------------------------------------------------------------
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?%s", config.GlobalConfig.Server, parameters)

	dname := "yc-" + timestamp
	if len(config.GlobalConfig.StoragePath) > 0 {
		dname = filepath.Join(config.GlobalConfig.StoragePath, dname)
	}
	err = os.Mkdir(dname, 0777)
	if err != nil {
		return
	}
	if config.GlobalConfig.DeferDelete {
		wg.Add(1)
		defer func() {
			defer wg.Done()
			err := os.RemoveAll(dname)
			if err != nil {
				logger.Log("WARNING: Can not remove the current directory: %s", err)
				return
			}
		}()
	}
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	defer func() {
		err := os.Chdir(dir)
		if err != nil {
			logger.Log("WARNING: Can not chdir: %s", err)
			return
		}
		if config.GlobalConfig.OnlyCapture {
			name, err := zipFolder(dname)
			if err != nil {
				logger.Log("WARNING: Can not zip folder: %s", err)
				return
			}
			logger.StdLog("All dumps can be found in %s", name)
			if logger.Log2File {
				logger.Log("All dumps can be found in %s", name)
			}
		}
	}()
	err = os.Chdir(dname)
	if err != nil {
		return
	}

	if !config.GlobalConfig.M3 {
		agentLogFile, err = logger.StartWritingToFile("agentlog.out")
		if err != nil {
			logger.Info().Err(err).Msg("Failed to start writing to file")
		}
	}
	startupLogs()

	startTime := time.Now()
	gcPath := config.GlobalConfig.GCPath
	tdPath := config.GlobalConfig.ThreadDumpPath
	hdPath := config.GlobalConfig.HeapDumpPath
	updatePaths(pid, &gcPath, &tdPath, &hdPath)
	pidPassed := true
	if pid <= 0 {
		pidPassed = false
	}

	var dockerID string
	if pidPassed {
		// find gc log path in from command line arguments of ps result
		if len(gcPath) < 1 {
			output, err := getGCLogFile(pid)
			if err == nil && len(output) > 0 {
				gcPath = output
			}
		}

		dockerID, _ = shell.GetDockerID(pid)
	}

	logger.Log("PID is %d", pid)
	logger.Log("YC_SERVER is %s", config.GlobalConfig.Server)
	logger.Log("API_KEY is %s", config.GlobalConfig.ApiKey)
	logger.Log("APP_NAME is %s", appName)
	logger.Log("JAVA_HOME is %s", config.GlobalConfig.JavaHomePath)
	logger.Log("GC_LOG is %s", gcPath)
	if len(dockerID) > 0 {
		logger.Log("DOCKER_ID is %s", dockerID)
	}

	// Display the PIDs which have been input to the script
	logger.Log("PROBLEMATIC_PID is: %d", pid)

	// Display the being used in this script
	logger.Log("SCRIPT_SPAN = %d", shell.SCRIPT_SPAN)
	logger.Log("JAVACORE_INTERVAL = %d", shell.JAVACORE_INTERVAL)
	logger.Log("TOP_INTERVAL = %d", shell.TOP_INTERVAL)
	logger.Log("TOP_DASH_H_INTERVAL = %d", shell.TOP_DASH_H_INTERVAL)
	logger.Log("VMSTAT_INTERVAL = %d", shell.VMSTAT_INTERVAL)

	// -------------------------------
	//     Transmit MetaInfo
	// -------------------------------
	msg, ok, err := writeMetaInfo(pid, appName, endpoint, tags)
	logger.Log(
		`META INFO DATA
Is transmission completed: %t
Resp: %s
Ignored errors: %v

--------------------------------
`, ok, msg, err)

	if pidPassed && !shell.IsProcessExists(pid) {
		defer func() {
			logger.Log("WARNING: Process %d doesn't exist.", pid)
			logger.Log("WARNING: You have entered non-existent processId. Please enter valid process id")
		}()
	}

	// check if it can find gc log from ps
	var gc *os.File
	gc, err = processGCLogFile(gcPath, "gc.log", dockerID, pid)
	if err != nil {
		logger.Log("process log file failed %s, err: %s", gcPath, err.Error())
	}
	var jstat shell.CmdManager
	var triedJAttachGC bool
	if pidPassed && (err != nil || gc == nil) {
		gc, jstat, err = shell.CommandStartInBackgroundToFile("gc.log",
			shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"}, shell.SudoHooker{PID: pid})
		if err == nil {
			gcPath = "gc.log"
			logger.Log("gc log set to %s", gcPath)
		} else {
			triedJAttachGC = true
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach...", err.Error())
			gc, jstat, err = captureGC(pid, gc, "gc.log")
			if err == nil {
				gcPath = "gc.log"
				logger.Log("jattach gc log set to %s", gcPath)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
		}
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()

	var capNetStat *capture.NetStat
	var netStat chan capture.Result
	var capTop *capture.Top
	var top chan capture.Result
	var capVMStat *capture.VMStat
	var vmstat chan capture.Result
	var dmesg chan capture.Result
	var threadDump chan capture.Result
	var capPS *capture.PS
	var ps chan capture.Result
	var disk chan capture.Result
	if pidPassed {
		// ------------------------------------------------------------------------------
		//                   Capture netstat x2
		// ------------------------------------------------------------------------------
		//  Collect the first netstat: date at the top, data, and then a blank line
		logger.Log("Collecting the first netstat snapshot...")
		capNetStat = capture.NewNetStat()
		netStat = goCapture(endpoint, capture.WrapRun(capNetStat))
		logger.Log("First netstat snapshot complete.")

		// ------------------------------------------------------------------------------
		//                   Capture top
		// ------------------------------------------------------------------------------
		//  It runs in the background so that other tasks can be completed while this runs.
		logger.Log("Starting collection of top data...")
		capTop = &capture.Top{}
		top = goCapture(endpoint, capture.WrapRun(capTop))
		logger.Log("Collection of top data started.")

		// ------------------------------------------------------------------------------
		//                   Capture vmstat
		// ------------------------------------------------------------------------------
		//  It runs in the background so that other tasks can be completed while this runs.
		logger.Log("Starting collection of vmstat data...")
		capVMStat = &capture.VMStat{}
		vmstat = goCapture(endpoint, capture.WrapRun(capVMStat))
		logger.Log("Collection of vmstat data started.")

		logger.Log("Collecting ps snapshot...")
		capPS = capture.NewPS()
		ps = goCapture(endpoint, capture.WrapRun(capPS))
		logger.Log("Collected ps snapshot.")

		// ------------------------------------------------------------------------------
		//  				Capture dmesg
		// ------------------------------------------------------------------------------
		logger.Log("Collecting other data.  This may take a few moments...")
		dmesg = goCapture(endpoint, capture.WrapRun(&capture.DMesg{}), capVMStat)
		// ------------------------------------------------------------------------------
		//  				Capture Disk Usage
		// ------------------------------------------------------------------------------
		disk = goCapture(endpoint, capture.WrapRun(&capture.Disk{}))

		logger.Log("Collected other data.")
	}

	// ------------------------------------------------------------------------------
	//   				Capture ping
	// ------------------------------------------------------------------------------
	ping := goCapture(endpoint, capture.WrapRun(&capture.Ping{Host: config.GlobalConfig.PingHost}))

	// ------------------------------------------------------------------------------
	//   				Capture kernel params
	// ------------------------------------------------------------------------------
	kernel := goCapture(endpoint, capture.WrapRun(&capture.Kernel{}))

	// ------------------------------------------------------------------------------
	//   				Capture thread dumps
	// ------------------------------------------------------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid:      pid,
		TdPath:   tdPath,
		JavaHome: config.GlobalConfig.JavaHomePath,
	}
	threadDump = goCapture(endpoint, capture.WrapRun(capThreadDump))

	// ------------------------------------------------------------------------------
	//   				Capture app log
	// ------------------------------------------------------------------------------
	var appLog chan capture.Result
	if len(config.GlobalConfig.AppLog) > 0 && config.GlobalConfig.AppLogLineCount > 0 {
		appLog = goCapture(endpoint, capture.WrapRun(&capture.AppLog{Path: config.GlobalConfig.AppLog, N: config.GlobalConfig.AppLogLineCount}))
	}

	// ------------------------------------------------------------------------------
	//   				Capture hdsub log
	// ------------------------------------------------------------------------------
	hdsubLog := goCapture(endpoint, capture.WrapRun(&capture.HDSub{
		Pid:      pid,
		JavaHome: config.GlobalConfig.JavaHomePath,
	}))

	// ------------------------------------------------------------------------------
	//                Capture final netstat
	// ------------------------------------------------------------------------------
	if capNetStat != nil {
		logger.Log("Collecting the final netstat snapshot...")
		capNetStat.Done()
		logger.Log("Final netstat snapshot complete.")
	}

	if jstat != nil {
		err := jstat.Wait()
		if err != nil && !triedJAttachGC {
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach......", err.Error())
			gc, jstat, err = captureGC(pid, gc, "gc.log")
			if err == nil {
				gcPath = "gc.log"
				logger.Log("jattach gc log set to %s", gcPath)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
			err = jstat.Wait()
			if err != nil {
				logger.Log("jattach gc log failed cause %s", err.Error())
			}
		}
	}
	// stop started tasks
	if capTop != nil {
		capTop.Kill()
	}
	if capVMStat != nil {
		capVMStat.Kill()
	}

	// -------------------------------
	//     Transmit Top data
	// -------------------------------
	if top != nil {
		result := <-top
		logger.Log(
			`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit DF data
	// -------------------------------
	if disk != nil {
		result := <-disk
		logger.Log(
			`DISK USAGE DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit netstat data
	// -------------------------------
	if netStat != nil {
		result := <-netStat
		logger.Log(
			`NETSTAT DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit ps data
	// -------------------------------
	if ps != nil {
		result := <-ps
		logger.Log(
			`PROCESS STATUS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit VMstat data
	// -------------------------------
	if vmstat != nil {
		result := <-vmstat
		logger.Log(
			`VMstat DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit DMesg data
	// -------------------------------
	if dmesg != nil {
		result := <-dmesg
		logger.Log(
			`DMesg DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok = shell.PostData(endpoint, "gc", gc)
	absGCPath, err := filepath.Abs(gcPath)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", gcPath, err.Error())
	}
	logger.Log(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absGCPath, ok, msg)

	// -------------------------------
	//     Transmit ping dump
	// -------------------------------
	if ping != nil {
		result := <-ping
		logger.Log(
			`PING DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit app log
	// -------------------------------
	if appLog != nil {
		result := <-appLog
		logger.Log(
			`APPLOG DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit hdsub log
	// -------------------------------
	if hdsubLog != nil {
		result := <-hdsubLog
		logger.Log(
			`HDSUB DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit kernel param dump
	// -------------------------------
	if kernel != nil {
		result := <-kernel
		logger.Log(
			`KERNEL PARAMS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit Thread dump
	// -------------------------------
	absTDPath, err := filepath.Abs(tdPath)
	if err != nil {
		absTDPath = fmt.Sprintf("path %s: %s", tdPath, err.Error())
	}
	if threadDump != nil {
		result := <-threadDump
		logger.Log(
			`THREAD DUMP DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absTDPath, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit Heap dump result
	// -------------------------------
	ep := fmt.Sprintf("%s/yc-receiver-heap?%s", config.GlobalConfig.Server, parameters)
	capHeapDump := capture.NewHeapDump(config.GlobalConfig.JavaHomePath, pid, hdPath, hd)
	capHeapDump.SetEndpoint(ep)
	hdResult, err := capHeapDump.Run()
	if err != nil {
		hdResult.Msg = fmt.Sprintf("capture heap dump failed: %s", err.Error())
		err = nil
	}
	logger.Log(
		`HEAP DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, hdResult.Ok, hdResult.Msg)

	// ------------------------------------------------------------------------------
	//  				Execute custom commands
	// ------------------------------------------------------------------------------
	logger.Log("Executing custom commands")
	for i, command := range config.GlobalConfig.Commands {
		customCmd := capture.Custom{
			Index:     i,
			UrlParams: string(command.UrlParams),
			Command:   cmdline.Split(string(command.Cmd)),
		}
		customCmd.SetEndpoint(endpoint)
		result, err := customCmd.Run()
		if err != nil {
			logger.Log("WARNING: Failed to execute custom command %d:%s, cause: %s", i, command.Cmd, err.Error())
			continue
		}
		logger.Log(
			`CUSTOM CMD %d: %s
Is transmission completed: %t
Resp: %s

--------------------------------
`, i, command.Cmd, result.Ok, result.Msg)
	}
	logger.Log("Executed custom commands")

	if config.GlobalConfig.OnlyCapture {
		if agentLogFile != nil {
			err := logger.StopWritingToFile()
			if err != nil {
				logger.Info().Err(err).Msg("Failed to stop writing to file")
			}
			agentLogFile = nil
		}
		return
	}
	// -------------------------------
	//     Conclusion
	// -------------------------------
	finEp := fmt.Sprintf("%s/yc-fin?%s", config.GlobalConfig.Server, parameters)
	resp, err := requestFin(finEp)
	if err != nil {
		logger.Log("post yc-fin err %s", err.Error())
		err = nil
	}

	endTime := time.Now()
	var result string
	rUrl, result = printResult(true, endTime.Sub(startTime).String(), resp)
	//	logger.StdLog(`
	//%s
	//`, resp)
	//	logger.StdLog(`
	//%s
	//`, result)
	logger.Log(`
%s
`, resp)
	logger.Log(`
%s
`, pterm.RemoveColorFromString(result))

	if agentLogFile != nil {
		msg, ok = shell.PostData(endpoint, "agentlog", agentLogFile)
		err := logger.StopWritingToFile()
		if err != nil {
			logger.Info().Err(err).Msg("Failed to stop writing to file")
		}
		agentLogFile = nil
		logger.Log(
			`AGENT LOG DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}
	return
}

func requestFin(endpoint string) (resp []byte, err error) {
	if config.GlobalConfig.OnlyCapture {
		err = errors.New("in only capture mode")
		return
	}
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !config.GlobalConfig.VerifySSL,
	}
	path := config.GlobalConfig.CACertPath
	if len(path) > 0 {
		pool := x509.NewCertPool()
		var ca []byte
		ca, err = ioutil.ReadFile(path)
		if err != nil {
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("POST", endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "text")
	req.Header.Set("ApiKey", config.GlobalConfig.ApiKey)
	post, err := httpClient.Do(req)
	if err == nil {
		defer post.Body.Close()
		resp, err = ioutil.ReadAll(post.Body)
		if err == nil {
			logger.Log(
				`yc-fin endpoint: %s
Resp: %s

--------------------------------
`, endpoint, resp)
		}
	}
	return
}

var getOutboundIP = shell.GetOutboundIP
var goCapture = capture.GoCapture

func getGCLogFile(pid int) (result string, err error) {
	output, err := shell.CommandCombinedOutput(shell.Append(shell.GC, fmt.Sprintf(`ps -f -p %d`, pid)))
	if err != nil {
		return
	}
	re := regexp.MustCompile("-Xlog:gc.+? ")
	loggc := re.Find(output)

	var fp []byte
	if len(loggc) > 1 {
		fre := regexp.MustCompile("file=(.+?)[: ]")
		submatch := fre.FindSubmatch(loggc)
		if len(submatch) >= 2 {
			fp = submatch[1]
		} else {
			fre := regexp.MustCompile("gc:((.+?)$|(.+?):)")
			submatch := fre.FindSubmatch(loggc)
			if len(submatch) >= 2 {
				fp = submatch[1]
			}
		}
	} else {
		re := regexp.MustCompile("-Xloggc:(.+?) ")
		submatch := re.FindSubmatch(output)
		if len(submatch) >= 2 {
			fp = submatch[1]
		}
	}
	if len(fp) < 1 {
		return
	}
	result = strings.TrimSpace(string(fp))
	return
}

func processGCLogFile(gcPath string, out string, dockerID string, pid int) (gc *os.File, err error) {
	if len(gcPath) <= 0 {
		return
	}
	// -Xloggc:/app/boomi/gclogs/gc%t.log
	if strings.Contains(gcPath, `%t`) {
		d := filepath.Dir(gcPath)
		open, err := os.Open(d)
		if err != nil {
			return nil, err
		}
		defer open.Close()
		fs, err := open.Readdirnames(0)
		if err != nil {
			return nil, err
		}

		var t time.Time
		var tf string
		for _, f := range fs {
			stat, err := os.Stat(filepath.Join(d, f))
			if err != nil {
				continue
			}
			mt := stat.ModTime()
			if t.IsZero() || mt.After(t) {
				t = mt
				tf = f
			}
		}
		if len(tf) > 0 {
			gcPath = filepath.Join(d, tf)
		}
	}
	// -Xloggc:/home/ec2-user/buggyapp/gc.%p.log
	// /home/ec2-user/buggyapp/gc.pid2843.log
	if strings.Contains(gcPath, `%p`) {
		gcPath = strings.Replace(gcPath, `%p`, "pid"+strconv.Itoa(pid), 1)
	}
	if len(dockerID) > 0 {
		err = shell.DockerCopy(out, dockerID+":"+gcPath)
		if err == nil {
			gc, err = os.Open(out)
			return
		}
	} else {
		gc, err = os.Create(out)
		if err != nil {
			return
		}
		err = copyFile(gc, gcPath, pid)
		if err == nil {
			return
		}
	}
	logger.Log("collecting rotation gc logs, because file open failed %s", err.Error())
	// err is other than not exists
	if !os.IsNotExist(err) {
		return
	}

	// config.GlobalConfig.GCPath is not exists, maybe using -XX:+UseGCLogFileRotation
	d := filepath.Dir(gcPath)
	logName := filepath.Base(gcPath)
	var fs []string
	if len(dockerID) > 0 {
		output, err := shell.DockerExecute(dockerID, "ls", "-1", d)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(output))
		for scanner.Scan() {
			line := scanner.Text()
			line = strings.TrimSpace(line)
			fs = append(fs, line)
		}
	} else {
		open, err := os.Open(d)
		if err != nil {
			return nil, err
		}
		defer open.Close()
		fs, err = open.Readdirnames(0)
		if err != nil {
			return nil, err
		}
	}
	re := regexp.MustCompile(logName + "\\.([0-9]+?)\\.current")
	reo := regexp.MustCompile(logName + "\\.([0-9]+)")
	var rf []string
	files := make([]int, 0, len(fs))
	for _, f := range fs {
		r := re.FindStringSubmatch(f)
		if len(r) > 1 {
			rf = r
			continue
		}
		r = reo.FindStringSubmatch(f)
		if len(r) > 1 {
			p, err := strconv.Atoi(r[1])
			if err != nil {
				logger.Log("skipped file %s because can not parse its index", f)
				continue
			}
			files = append(files, p)
		}
	}
	if len(rf) < 2 {
		err = fmt.Errorf("can not find the current log file, %w", os.ErrNotExist)
		return
	}
	p, err := strconv.Atoi(rf[1])
	if err != nil {
		return
	}
	// try to find previous log
	var preLog string
	if len(files) == 1 {
		preLog = gcPath + "." + strconv.Itoa(files[0])
	} else if len(files) > 1 {
		files = append(files, p)
		sort.Ints(files)
		index := -1
		for i, file := range files {
			if file == p {
				index = i
				break
			}
		}
		if index >= 0 {
			if index-1 >= 0 {
				preLog = gcPath + "." + strconv.Itoa(files[index-1])
			} else {
				preLog = gcPath + "." + strconv.Itoa(files[len(files)-1])
			}
		}
	}
	if gc == nil {
		gc, err = os.Create(out)
		if err != nil {
			return
		}
	}
	if len(preLog) > 0 {
		logger.Log("collecting previous gc log %s", preLog)
		if len(dockerID) > 0 {
			tmp := filepath.Join(os.TempDir(), out+".pre")
			err = shell.DockerCopy(tmp, dockerID+":"+preLog)
			if err == nil {
				err = copyFile(gc, tmp, pid)
			}
		} else {
			err = copyFile(gc, preLog, pid)
		}
		if err != nil {
			logger.Log("failed to collect previous gc log %s", err.Error())
		} else {
			logger.Log("collected previous gc log %s", preLog)
		}
	}

	curLog := filepath.Join(d, rf[0])
	logger.Log("collecting previous gc log %s", curLog)
	if len(dockerID) > 0 {
		tmp := filepath.Join(os.TempDir(), out+".cur")
		err = shell.DockerCopy(tmp, dockerID+":"+curLog)
		if err == nil {
			err = copyFile(gc, tmp, pid)
		}
	} else {
		err = copyFile(gc, curLog, pid)
	}
	if err != nil {
		logger.Log("failed to collect previous gc log %s", err.Error())
	} else {
		logger.Log("collected previous gc log %s", curLog)
	}
	return
}

// combine previous gc log to new gc log
func copyFile(gc *os.File, file string, pid int) (err error) {
	log, err := os.Open(file)
	if err != nil && runtime.GOOS == "linux" {
		logger.Log("Failed to %s. Trying to open in the Docker container...", err)
		log, err = os.Open(filepath.Join("/proc", strconv.Itoa(pid), "root", file))
	}
	if err != nil {
		return
	}
	defer log.Close()
	_, err = io.Copy(gc, log)
	return
}

const metaInfoTemplate = `hostName=%s
processId=%d
appName=%s
whoami=%s
javaVersion=%s
osVersion=%s
tags=%s`

func writeMetaInfo(processId int, appName, endpoint, tags string) (msg string, ok bool, err error) {
	file, err := os.Create("meta-info.txt")
	if err != nil {
		return
	}
	defer file.Close()
	hostname, e := os.Hostname()
	if e != nil {
		err = fmt.Errorf("hostname err: %v", e)
	}
	var jv string
	javaVersion, e := shell.CommandCombinedOutput(shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/java"), "-version"})
	if e != nil {
		err = fmt.Errorf("javaVersion err: %v, previous err: %v", e, err)
	} else {
		jv = strings.ReplaceAll(string(javaVersion), "\r\n", ", ")
		jv = strings.ReplaceAll(jv, "\n", ", ")
	}
	var ov string
	osVersion, e := shell.CommandCombinedOutput(shell.OSVersion)
	if e != nil {
		err = fmt.Errorf("osVersion err: %v, previous err: %v", e, err)
	} else {
		ov = strings.ReplaceAll(string(osVersion), "\r\n", ", ")
		ov = strings.ReplaceAll(ov, "\n", ", ")
	}
	var un string
	current, e := user.Current()
	if e != nil {
		err = fmt.Errorf("username err: %v, previous err: %v", e, err)
	} else {
		un = current.Username
	}
	_, e = file.WriteString(fmt.Sprintf(metaInfoTemplate, hostname, processId, appName, un, jv, ov, tags))
	if e != nil {
		err = fmt.Errorf("write result err: %v, previous err: %v", e, err)
		return
	}
	msg, ok = shell.PostData(endpoint, "meta", file)
	return
}
