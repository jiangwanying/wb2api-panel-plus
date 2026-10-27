//go:build !windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func setHiddenWindow(_ *exec.Cmd) {}

func waitForProcessExit(pid int, timeout time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return os.ErrDeadlineExceeded
}

func launchCleanup(_, _ string, _ int) {}

func processSupervised() bool {
	if runtime.GOOS == "linux" {
		return os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" || os.Getenv("RC_SVCNAME") != ""
	}
	return runtime.GOOS == "darwin" && os.Getenv("LAUNCH_JOBKEY_LABEL") != ""
}

// waitChildHealthy 等一个刚拉起的子进程活过观察窗口：窗口内退出返回错误（调用
// 方据此回滚旧版本），活过窗口返回 nil。unix 用 Wait4+WNOHANG 轮询并顺手回收，
// 僵尸进程不会干扰「还活着吗」的判断（Signal(0) 对僵尸也返回成功，不能用）。
func waitChildHealthy(cmd *exec.Cmd, window time.Duration) error {
	deadline := time.Now().Add(window)
	pid := cmd.Process.Pid
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == nil && wpid == pid {
			return fmt.Errorf("子进程 %d 在观察窗口内退出（exit=%v）", pid, ws.ExitStatus())
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}
