// Package importer imports history from a Signal on-device backup: the zip of a phone's SignalBackups folder, with
// the account's recovery key.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
)

// The OpenHost router buffers each request body in memory, so uploads come in chunks of at most this size.
const MaxChunkBytes = 8 << 20

type State string

const (
	StateIdle      State = "idle"
	StateUploading State = "uploading"
	StateReady     State = "ready"
	StateImporting State = "importing"
	StateDone      State = "done"
	StateFailed    State = "failed"
)

// Counts is what an import did.
type Counts struct {
	Messages    int `json:"messages"`
	Skipped     int `json:"skipped"`
	Attachments int `json:"attachments"`
	Chats       int `json:"chats"`
}

// Status is the import's progress, as shown in settings and pushed as "import" events.
type Status struct {
	State State `json:"state"`
	// While uploading: bytes received so far, of Total.
	Received int64 `json:"received"`
	Total    int64 `json:"total"`
	// While importing: how far through the backup, 0 to 1.
	Progress float64 `json:"progress"`
	Counts   Counts  `json:"counts"`
	Error    string  `json:"error,omitempty"`
	// The recovery key was wrong: the upload is kept, to try again with another.
	WrongKey bool `json:"wrong_key,omitempty"`
}

var (
	ErrBusy        = errors.New("an import is already running")
	ErrWrongOffset = errors.New("chunk doesn't continue the upload")
	ErrNotReady    = errors.New("the upload isn't complete")
	ErrNotLoggedIn = errors.New("register this server's Signal account before importing, so chats can be matched to it")
)

// userError is a problem with what the user uploaded or asked for, rather than a failure here.
type userError struct{ err error }

func (e userError) Error() string { return e.err.Error() }
func (e userError) Unwrap() error { return e.err }

// IsUserError reports whether err is about what the user did or uploaded.
func IsUserError(err error) bool {
	for _, sentinel := range []error{ErrBusy, ErrWrongOffset, ErrNotReady, ErrNotLoggedIn, backup.ErrWrongRecoveryKey} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return errors.As(err, new(userError)) || errors.As(err, new(backup.InvalidBackupError))
}

// Manager runs one import at a time. An upload in progress survives restarts (it resumes from the size on disk).
type Manager struct {
	dir            string
	attachmentsDir string
	svc            *bridge.Service
	st             *store.Store
	fs             *files.Store
	broker         *events.Broker
	log            zerolog.Logger

	mu     sync.Mutex
	status Status
}

type uploadMeta struct {
	Total int64 `json:"total"`
}

func NewManager(dir, attachmentsDir string, svc *bridge.Service, st *store.Store, fs *files.Store, broker *events.Broker, log zerolog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{
		dir: dir, attachmentsDir: attachmentsDir, svc: svc, st: st, fs: fs, broker: broker, log: log,
		status: Status{State: StateIdle},
	}
	var meta uploadMeta
	if data, err := os.ReadFile(m.metaPath()); err == nil {
		if err = json.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("reading upload state: %w", err)
		}
		m.status = Status{State: StateUploading, Received: m.received(), Total: meta.Total}
		if m.status.Received == meta.Total {
			m.status.State = StateReady
		}
	}
	return m, nil
}

func (m *Manager) zipPath() string  { return filepath.Join(m.dir, "backup.zip") }
func (m *Manager) metaPath() string { return filepath.Join(m.dir, "backup.json") }

func (m *Manager) received() int64 {
	info, err := os.Stat(m.zipPath())
	if err != nil {
		return 0
	}
	return info.Size()
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) setLocked(s Status) {
	m.status = s
	m.broker.Publish("import", s)
}

// freeBytes is the space left on the filesystem holding dir.
func freeBytes(dir string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}

// checkSpace refuses an upload there isn't room for: the zip itself, and its attachments once decrypted (about the
// zip's size again, at most).
func (m *Manager) checkSpace(total int64) error {
	for _, dir := range []string{m.dir, m.attachmentsDir} {
		free, err := freeBytes(dir)
		if err != nil {
			return err
		}
		if free < total+(256<<20) {
			return userError{fmt.Errorf("not enough free space: the backup needs about %d MB, and there are %d MB free",
				total>>20, free>>20)}
		}
	}
	return nil
}

// Start begins a new upload of total bytes, discarding any previous one.
func (m *Manager) Start(total int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == StateImporting {
		return ErrBusy
	}
	if total <= 0 {
		return userError{errors.New("invalid size")}
	}
	if err := m.removeFiles(); err != nil {
		return err
	}
	if err := m.checkSpace(total); err != nil {
		return err
	}
	data, err := json.Marshal(uploadMeta{Total: total})
	if err != nil {
		return err
	}
	if err = os.WriteFile(m.metaPath(), data, 0o600); err != nil {
		return err
	}
	if err = os.WriteFile(m.zipPath(), nil, 0o600); err != nil {
		return err
	}
	m.setLocked(Status{State: StateUploading, Total: total})
	return nil
}

// Append adds a chunk at offset, which must be the number of bytes received so far.
func (m *Manager) Append(offset int64, chunk io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State != StateUploading {
		return ErrNotReady
	}
	if offset != m.status.Received {
		return ErrWrongOffset
	}
	f, err := os.OpenFile(m.zipPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(chunk, MaxChunkBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > MaxChunkBytes || offset+n > m.status.Total {
		// Drop a partial or oversized write, so the client can resend the chunk.
		_ = os.Truncate(m.zipPath(), offset)
		if err = errors.Join(copyErr, closeErr); err == nil {
			err = userError{errors.New("chunk too large or past the end of the upload")}
		}
		return err
	}
	m.status.Received = offset + n
	if m.status.Received == m.status.Total {
		m.status.State = StateReady
	}
	m.broker.Publish("import", m.status)
	return nil
}

// Run imports the complete upload in the background, with the backup's recovery key.
func (m *Manager) Run(ctx context.Context, recoveryKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.status.State {
	case StateReady:
	case StateFailed:
		if m.received() == 0 {
			return ErrNotReady
		}
	case StateImporting:
		return ErrBusy
	default:
		return ErrNotReady
	}
	if m.svc.CurrentLogin(ctx) == nil {
		return ErrNotLoggedIn
	}
	if _, err := backup.NormalizeRecoveryKey(recoveryKey); err != nil {
		return err
	}
	m.setLocked(Status{State: StateImporting, Received: m.status.Total, Total: m.status.Total})
	go m.run(recoveryKey)
	return nil
}

func (m *Manager) run(recoveryKey string) {
	ctx := m.log.WithContext(context.Background())
	r := &run{m: m, progress: func(fraction float64, counts Counts) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.status.Progress, m.status.Counts = fraction, counts
		m.broker.Publish("import", m.status)
	}}
	err := r.importZip(ctx, m.zipPath(), recoveryKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.Counts = r.counts
	switch {
	case errors.Is(err, backup.ErrWrongRecoveryKey):
		s.State, s.Error, s.WrongKey = StateFailed, err.Error(), true
	case err != nil:
		m.log.Err(err).Msg("Import failed")
		s.State, s.Error = StateFailed, err.Error()
	default:
		s.State, s.Progress = StateDone, 1
		m.log.Info().Interface("counts", r.counts).Msg("Import done")
	}
	// Only a wrong key can be fixed by trying the same upload again.
	if !s.WrongKey {
		if err = m.removeFiles(); err != nil {
			m.log.Err(err).Msg("Failed to delete the imported backup")
		}
	}
	m.setLocked(s)
}

// Discard drops the upload.
func (m *Manager) Discard() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == StateImporting {
		return ErrBusy
	}
	if err := m.removeFiles(); err != nil {
		return err
	}
	m.setLocked(Status{State: StateIdle})
	return nil
}

func (m *Manager) removeFiles() error {
	for _, p := range []string{m.zipPath(), m.metaPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
