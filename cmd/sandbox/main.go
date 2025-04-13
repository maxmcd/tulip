//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	// The path to our new root
	newRoot := "/tmp/crfs/rootfs"

	// Command to run in the new namespace
	cmd := exec.Command("/bin/busybox", "sh")
	fmt.Println(cmd)
	cmd.Path = "/bin/busybox"

	// Set up the command to use a new mount namespace
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS,
		// Unshare user namespace if needed (may be required for non-root)
		// Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER,
	}

	// Set up stdio
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// This will be run before exec-ing the command
	cmd.SysProcAttr.Chroot = newRoot

	// Optional: set working directory inside chroot
	cmd.Dir = "/"

	// Start the process
	if err := cmd.Start(); err != nil {
		fmt.Printf("Failed to start process: %v\n", err)
		return
	}

	// Wait for the process to complete
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		} else {
			fmt.Printf("Command finished with error: %v\n", err)
		}
	}

}
