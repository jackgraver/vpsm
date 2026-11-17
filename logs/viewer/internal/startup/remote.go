package startup

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// remote runs `ssh <host> mini-bot logs <args>` and streams its output.
//
// The viewer runs ssh itself rather than being piped from it because in
// `ssh host ... | vpsm-logs` both programs read the terminal: ssh forwards
// keystrokes to the remote command, and each key goes to whichever reads
// first. Here ssh's stdin is /dev/null, so every key reaches the viewer,
// and quitting stops ssh at once instead of when the next line arrives.
type remote struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *syncBuffer
}

// mini-bot logs arguments for a viewer invocation.
func remoteArgs(since string, sources []string, follow bool) []string {
	args := []string{"mini-bot", "logs"}
	if since != "" {
		args = append(args, "--since", since)
	}
	for _, s := range sources {
		args = append(args, "--source", s)
	}
	if follow {
		args = append(args, "--follow")
	}
	return args
}

func startRemote(host string, args []string) (*remote, error) {
	// ssh joins its arguments into one remote shell command, so each is
	// quoted: `-since "2026-11-16 10:00"` must stay one argument.
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	cmd := exec.Command("ssh", host, strings.Join(quoted, " "))
	// ssh's own errors (unknown host, auth, mini-bot not installed) would
	// scribble over the viewer, so they are kept and printed after it.
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &remote{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// stop waits for ssh, first ending it unless its output already reached
// the end (ended). It returns what ssh said on stderr, and whether ssh
// failed on its own: being stopped because the viewer quit is not a
// failure.
func (r *remote) stop(ended bool) (string, bool) {
	if !ended {
		_ = r.cmd.Process.Kill()
	}
	err := r.cmd.Wait()
	return strings.TrimSpace(r.stderr.String()), ended && err != nil
}

// shellQuote quotes s for a POSIX shell, leaving plain words as they are.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// syncBuffer is a bytes.Buffer safe to write from exec's stderr copier
// while being read after Wait.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
