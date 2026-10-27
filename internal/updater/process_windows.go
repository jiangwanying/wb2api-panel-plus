//go:build windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func setHiddenWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := windows.WaitForSingleObject(handle, 250)
		if err != nil {
			return err
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("等待旧进程退出失败")
		}
	}
	return os.ErrDeadlineExceeded
}

func launchCleanup(executable, path string, pid int) {
	if executable == "" || path == "" {
		return
	}
	cmd := exec.Command(executable, cleanupArg, strconv.Itoa(pid), path)
	setHiddenWindow(cmd)
	_ = startChild(cmd)
}

func processSupervised() bool {
	managed, err := svc.IsWindowsService()
	return err == nil && managed
}

// waitChildHealthy 等一个刚拉起的子进程活过观察窗口：窗口内退出返回错误（调用
// 方据此回滚旧版本），活过窗口返回 nil。Windows 按 pid 打开 SYNCHRONIZE 句柄做
// 0ms 探测轮询（WAIT_OBJECT_0 = 已退出，WAIT_TIMEOUT = 仍在运行），与
// waitForProcessExit 同款手法（os.Process 的句柄字段未导出，拿不到现成的）。
func waitChildHealthy(cmd *exec.Cmd, window time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return errors.New("新版本进程在观察窗口内退出")
		}
		return fmt.Errorf("观察新版本进程: %w", err)
	}
	defer windows.CloseHandle(handle)
	deadline := time.Now().Add(window)
	for {
		result, err := windows.WaitForSingleObject(handle, 0)
		if err != nil {
			return fmt.Errorf("观察新版本进程: %w", err)
		}
		if result == windows.WAIT_OBJECT_0 {
			return errors.New("新版本进程在观察窗口内退出")
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("观察新版本进程失败")
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}
