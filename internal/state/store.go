package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Store persists state to a local JSON file. Writes are atomic: the new
// document is written to a temporary file in the same directory, fsynced and
// renamed over the old one, so a crash never leaves a torn state file. The
// previous version is kept as <path>.backup.
type Store struct {
	Path string
}

// Load reads the state, returning a fresh empty state if the file does not
// exist.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("reading state %s: %w", s.Path, err)
	}
	return st, nil
}

// Save increments the serial and atomically writes st.
func (s *Store) Save(st *State) error {
	st.Serial++
	data, err := st.MarshalJSON() // direct call keeps the indented layout
	if err != nil {
		st.Serial--
		return err
	}
	data = append(data, '\n')
	if err := writeAtomic(s.Path, data); err != nil {
		st.Serial--
		return err
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".backup", old, 0o600); err != nil {
			cleanup()
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: persist the rename itself
		_ = d.Close()
	}
	return nil
}

// LockInfo describes the holder of a state lock.
type LockInfo struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Who       string    `json:"who"`
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	Created   time.Time `json:"created"`
}

// LockedError is returned when another process holds the lock.
type LockedError struct {
	Path string
	Info LockInfo
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("state is locked by %s (pid %d on %s) for %q since %s; lock ID %s\n"+
		"if that process is gone, run: plait force-unlock %s",
		e.Info.Who, e.Info.PID, e.Info.Host, e.Info.Operation, e.Info.Created.Format(time.RFC3339), e.Info.ID, e.Info.ID)
}

// Lock is a held state lock.
type Lock struct {
	path string
	Info LockInfo
	// BrokeStale is set when acquiring the lock removed a stale lock left by
	// a dead process on this host.
	BrokeStale *LockInfo
}

// LockPath returns the lock file path for the store.
func (s *Store) LockPath() string { return s.Path + ".lock" }

// Lock acquires the advisory lock for operation, retrying until timeout.
// A lock held by a process that no longer exists on this host is considered
// stale and is broken automatically (crash recovery); the returned Lock
// reports that in BrokeStale.
func (s *Store) Lock(operation string, timeout time.Duration) (*Lock, error) {
	host, _ := os.Hostname()
	who := os.Getenv("USER")
	if who == "" {
		who = "unknown"
	}
	info := LockInfo{ID: NewToken()[:12], Operation: operation, Who: who, PID: os.Getpid(), Host: host, Created: time.Now().UTC()}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.LockPath()), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var broke *LockInfo
	for {
		f, err := os.OpenFile(s.LockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.Write(data)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(s.LockPath())
				return nil, errors.Join(werr, cerr)
			}
			return &Lock{path: s.LockPath(), Info: info, BrokeStale: broke}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		held, rerr := s.ReadLock()
		if rerr != nil {
			if errors.Is(rerr, fs.ErrNotExist) {
				continue // released between our attempts
			}
			return nil, fmt.Errorf("lock file %s exists but is unreadable: %w", s.LockPath(), rerr)
		}
		if held.Host == host && held.PID > 0 && !processAlive(held.PID) {
			if err := os.Remove(s.LockPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			h := held
			broke = &h
			continue
		}
		if time.Now().After(deadline) {
			return nil, &LockedError{Path: s.LockPath(), Info: held}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ReadLock returns the current lock holder.
func (s *Store) ReadLock() (LockInfo, error) {
	var info LockInfo
	data, err := os.ReadFile(s.LockPath())
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(data, &info)
	return info, err
}

// ForceUnlock removes the lock if its ID matches.
func (s *Store) ForceUnlock(id string) error {
	held, err := s.ReadLock()
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("state is not locked")
	}
	if err != nil {
		return err
	}
	if held.ID != id {
		return fmt.Errorf("lock ID mismatch: state is locked with ID %s", held.ID)
	}
	return os.Remove(s.LockPath())
}

// Unlock releases the lock if this process still holds it.
func (l *Lock) Unlock() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("releasing lock: %w", err)
	}
	var held LockInfo
	if err := json.Unmarshal(data, &held); err != nil || held.ID != l.Info.ID {
		return fmt.Errorf("releasing lock: lock file no longer belongs to this process")
	}
	return os.Remove(l.path)
}

// processAlive reports whether pid exists, using signal 0.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
