// Package exporter writes everything here as a Signal on-device backup that a phone can restore during Signal's
// setup, with the account's recovery key: the way to move the account and its history back to a phone.
package exporter

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

type State string

const (
	StateIdle      State = "idle"
	StateExporting State = "exporting"
	StateDone      State = "done"
	StateFailed    State = "failed"
)

// Status is the export's progress, pushed as "export" events. The recovery key is only in the status asked for
// directly (Manager.Status with the key), not in the pushed events.
type Status struct {
	State State `json:"state"`
	// Identifies a finished export, in its download URL.
	ID     string `json:"id,omitempty"`
	Counts Counts `json:"counts"`
	// The backup's size on disk; the zip is about the same.
	Size        int64  `json:"size,omitempty"`
	RecoveryKey string `json:"recovery_key,omitempty"`
	Error       string `json:"error,omitempty"`
}

var (
	ErrBusy        = errors.New("an export is already running")
	ErrDownloading = errors.New("the backup is being downloaded; try again when the download is done")
	ErrNotLoggedIn = errors.New("this server has no Signal account to export")
)

// Manager runs one export at a time, and keeps its result until it's deleted or replaced.
type Manager struct {
	dir       string
	signalDir string
	svc       *bridge.Service
	st        *store.Store
	fs        *files.Store
	broker    *events.Broker
	log       zerolog.Logger

	mu     sync.Mutex
	status Status
	// Downloads of the finished export in progress, which mustn't lose their files.
	downloads int
	// The finished export's SignalBackups folder.
	backupDir string
}

func NewManager(dir, signalDir string, svc *bridge.Service, st *store.Store, fs *files.Store, broker *events.Broker, log zerolog.Logger) (*Manager, error) {
	// Exports don't survive restarts: start clean.
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{
		dir: dir, signalDir: signalDir, svc: svc, st: st, fs: fs, broker: broker, log: log,
		status: Status{State: StateIdle},
	}, nil
}

// Status is the export's status, with the recovery key once it's done.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) publishLocked() {
	s := m.status
	s.RecoveryKey = ""
	m.broker.Publish("export", s)
}

// Start begins an export in the background, replacing a previous one.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == StateExporting {
		return ErrBusy
	} else if m.downloads > 0 {
		return ErrDownloading
	}
	if m.svc.CurrentLogin(ctx) == nil {
		return ErrNotLoggedIn
	}
	if err := m.clearLocked(); err != nil {
		return err
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	m.status = Status{State: StateExporting, ID: id}
	m.publishLocked()
	go m.run(id)
	return nil
}

func (m *Manager) run(id string) {
	ctx := m.log.WithContext(context.Background())
	e := &export{m: m, now: time.Now(), progress: func(c Counts) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.status.Counts = c
		m.publishLocked()
	}}
	dir := filepath.Join(m.dir, id)
	backupDir, err := e.run(ctx, dir)
	var size int64
	if err == nil {
		size, err = dirSize(backupDir)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Counts = e.counts
	if err != nil {
		m.log.Err(err).Msg("Export failed")
		m.status.State, m.status.Error = StateFailed, err.Error()
		_ = os.RemoveAll(dir)
	} else {
		m.log.Info().Interface("counts", e.counts).Msg("Export done")
		m.status.State, m.status.Size, m.status.RecoveryKey = StateDone, size, e.secrets.RecoveryKey
		m.backupDir = backupDir
	}
	m.publishLocked()
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	return total, err
}

// Delete removes the finished export.
func (m *Manager) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == StateExporting {
		return ErrBusy
	} else if m.downloads > 0 {
		return ErrDownloading
	}
	if err := m.clearLocked(); err != nil {
		return err
	}
	m.status = Status{State: StateIdle}
	m.publishLocked()
	return nil
}

func (m *Manager) clearLocked() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = os.RemoveAll(filepath.Join(m.dir, entry.Name())); err != nil {
			return err
		}
	}
	m.backupDir = ""
	return nil
}

// download is the SignalBackups folder of the finished export with this id, kept until done is called.
func (m *Manager) download(id string) (dir string, done func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State != StateDone || m.status.ID != id {
		return "", nil, store.ErrNotFound
	}
	m.downloads++
	return m.backupDir, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.downloads--
	}, nil
}

// writeZip writes a SignalBackups folder as a zip, as it's streamed. The files are encrypted, so they're stored rather
// than compressed.
func writeZip(backupDir string, w io.Writer) error {
	zw := zip.NewWriter(w)
	root := filepath.Dir(backupDir)
	err := filepath.WalkDir(backupDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name, header.Method = filepath.ToSlash(rel), zip.Store
		dst, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(dst, src)
		return err
	})
	return errors.Join(err, zw.Close())
}
