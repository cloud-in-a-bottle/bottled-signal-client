package signalcli

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

const daemonAddr = "127.0.0.1:7583"

// BaseURL is where the daemon serves its HTTP API.
const BaseURL = "http://" + daemonAddr

// RunDaemon runs `signal-cli daemon` until ctx ends, restarting it if it exits.
//
// It runs in multi-account mode, so that registering works before there is an account. With
// `--receive-mode on-connection`, messages stay queued on Signal's servers until an event stream is connected, rather
// than the daemon receiving (and acknowledging) them with nobody listening.
func RunDaemon(ctx context.Context, binary, dataDir string, log zerolog.Logger) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, binary, "--data-dir", dataDir, "daemon",
			"--http", daemonAddr, "--receive-mode", "on-connection", "--no-receive-stdout", "--ignore-stories")
		// Let it finish what it's writing; it's killed if it takes longer than WaitDelay.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		err := runLogged(cmd, log)
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("signal-cli daemon exited; restarting in 5s")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// runLogged runs cmd with its output (stdout and stderr together) going to log, line by line.
func runLogged(cmd *exec.Cmd, log zerolog.Logger) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("starting signal-cli: %w", err)
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		log.Info().Str("line", scanner.Text()).Msg("signal-cli")
	}
	return cmd.Wait()
}

// WaitUp waits for the daemon to answer.
func (c *Client) WaitUp(ctx context.Context) error {
	for !c.Up(ctx) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
}
