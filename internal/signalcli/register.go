package signalcli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// ErrCaptchaRequired means Signal wants a captcha solved before it sends a verification code.
var ErrCaptchaRequired = errors.New("Signal needs a captcha to be solved first")

// Register asks Signal to send number a verification code, by SMS or with voice by a call. It runs signal-cli's CLI
// rather than the daemon's JSON-RPC: the native build of 0.14.9 lacks the reflection metadata to read the JSON-RPC
// `register` parameters, so the daemon rejects them. The pending registration is kept in the account's file, where the
// daemon's `verify` finds it.
func Register(ctx context.Context, binary, dataDir, number, captcha string, voice bool) error {
	args := []string{"--data-dir", dataDir, "-a", number, "register"}
	if voice {
		args = append(args, "--voice")
	}
	if captcha != "" {
		args = append(args, "--captcha", strings.TrimSpace(captcha))
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// The last line is the error for the user; earlier ones are log noise.
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if strings.Contains(strings.ToLower(last), "captcha") && captcha == "" {
			return ErrCaptchaRequired
		} else if last == "" {
			return err
		}
		return &RPCError{Message: last}
	}
	return nil
}

// IsRegistrationLocked reports whether verify failed because the number has registration lock on and needs its PIN.
func IsRegistrationLocked(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && strings.Contains(rpcErr.Message, "locked with a pin")
}
