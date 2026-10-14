package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const absence = "fatal: not a git repository (or any of the parent directories): .git"

func logCall(event string) {
	f, err := os.OpenFile(os.Getenv("NM_GIT_FAULT_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%d %s locale=%s/%s\n", time.Now().UnixNano(), event, os.Getenv("LC_ALL"), os.Getenv("LANG"))
		_ = f.Close()
	}
}

func stall(event string) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	logCall(event + "-start")
	select {
	case <-stop:
		logCall(event + "-canceled")
		os.Exit(4)
	case <-time.After(30 * time.Second):
		os.Exit(5)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		ready := os.Getenv("NM_GIT_FAULT_READY")
		data := strconv.Itoa(os.Getpid()) + " " + os.Args[2]
		if err := os.WriteFile(ready, []byte(data), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(30 * time.Second)
		return
	}
	args := strings.Join(os.Args[1:], " ")
	common := strings.Contains(args, "--git-common-dir")
	top := strings.Contains(args, "--show-toplevel")
	logCall(args)
	switch os.Getenv("NM_GIT_FAULT_MODE") {
	case "top-stall":
		if top {
			stall("top")
		}
	case "shared-budget":
		if common {
			logCall("common-start")
			time.Sleep(4 * time.Second)
		}
		if top {
			stall("top")
		}
	case "common-stall":
		if common {
			stall("common")
		}
	case "top-error":
		if top {
			fmt.Fprintln(os.Stderr, os.Getenv("NM_GIT_FAULT_STDERR"))
			os.Exit(128)
		}
	case "error":
		fmt.Fprintln(os.Stderr, os.Getenv("NM_GIT_FAULT_STDERR"))
		os.Exit(128)
	case "escaped":
		executable, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		child := exec.Command(executable, "hold", os.Getenv("NM_GIT_FAULT_TOKEN"))
		child.Stderr = os.Stderr
		configureEscape(child)
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv("NM_GIT_FAULT_READY")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = child.Process.Kill()
				os.Exit(3)
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, absence)
		os.Exit(128)
	}
	cmd := exec.Command(os.Getenv("NM_GIT_FAULT_REAL"), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if common && os.Getenv("NM_GIT_FAULT_MODE") == "grow-gitfile" {
		file, err := os.OpenFile(os.Getenv("NM_GIT_FAULT_GITFILE"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			panic(err)
		}
		if _, err := file.WriteString(strings.Repeat("\n", (1<<20)+1)); err != nil {
			panic(err)
		}
		if err := file.Close(); err != nil {
			panic(err)
		}
	}

}
