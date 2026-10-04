package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

type wakeResult struct {
	wake bool
	err  error
}

func (r *relay) launchWakeProbe() {
	if len(r.wakeArgv) == 0 || r.wakeResults == nil || r.running || r.probeRunning || !r.initialized || len(r.initParams) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.wakeTimeout)
	r.probeCancel = cancel
	r.probeRunning = true
	argv := append([]string(nil), r.wakeArgv...)
	go func() {
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		configureProcess(cmd)
		cmd.Cancel = func() error { killProcessGroup(cmd); return nil }
		cmd.WaitDelay = 250 * time.Millisecond
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		err := cmd.Run()
		killProcessGroup(cmd)
		result := wakeResult{}
		if ctx.Err() != nil {
			result.err = fmt.Errorf("predicate deadline/cancellation: %w", ctx.Err())
		} else if err == nil {
			result.wake = true
		} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			result.err = fmt.Errorf("predicate exited unsuccessfully: %w", err)
		}
		r.wakeResults <- result
	}()
}

func (r *relay) stopWakeProbe() {
	if r.probeCancel != nil {
		r.probeCancel()
	}
	if r.probeRunning {
		select {
		case <-r.wakeResults:
		case <-time.After(time.Second):
			r.log("wake predicate cleanup did not finish")
		}
	}
}
