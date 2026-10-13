package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	dayLayout       = "2006-01-02"
	dayFilePrefix   = "usage-"
	spillFilePrefix = "spill-"
	dayFileSuffix   = ".json"
	dayFileVersion  = 1
	lockFileName    = ".lock"
	flushInterval   = 10 * time.Second
)

type rowKey struct {
	Hour     int
	Account  string
	Provider string
	Model    string
}

type row struct {
	counters
	sessions map[string]struct{}
}

// day holds one UTC day of hourly rows and the account names seen that day.
type day struct {
	accounts map[string]accountInfo
	rows     map[rowKey]*row
}

func newDay() *day {
	return &day{accounts: make(map[string]accountInfo), rows: make(map[rowKey]*row)}
}

func (d *day) addRow(key rowKey, c counters, sessions ...string) {
	r := d.rows[key]
	if r == nil {
		r = &row{sessions: make(map[string]struct{})}
		d.rows[key] = r
	}
	r.add(c)
	for _, session := range sessions {
		if session != "" {
			r.sessions[session] = struct{}{}
		}
	}
}

// dayFile is the on-disk form of a day: usage-<YYYY-MM-DD>.json.
type dayFile struct {
	Version  int                    `json:"version"`
	Date     string                 `json:"date"`
	Accounts map[string]accountInfo `json:"accounts,omitempty"`
	Rows     []rowFile              `json:"rows"`
}

type rowFile struct {
	Hour     int    `json:"hour"`
	Account  string `json:"account,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	counters
	Sessions []string `json:"sessions,omitempty"`
}

// store keeps usage in memory and mirrors it to day files in dir. Only the
// instance holding dir's lock loads and writes the day files. A plugin
// upgrade keeps the old library loaded, retired, until CLIProxyAPI exits:
// it is quiesced before the new one loads and may still finish a request
// afterwards. So a quiesced store gives the lock up, and a store without the
// lock writes what it collects to spill files that the lock holder merges.
type store struct {
	dir string
	now func() time.Time
	// instance names this store's spill files apart from other libraries'.
	instance string

	mu        sync.Mutex
	retention int
	// days holds every loaded day while the lock is held, and only usage not
	// yet spilled otherwise.
	days      map[string]*day
	dirty     map[string]bool
	loaded    bool
	quiesced  bool
	lock      *os.File
	lastError string
	lastPrune time.Time
	// mergedSpills are spill files already in days, deleted once written.
	mergedSpills map[string]bool
}

func newStore(dir string, retentionDays int) *store {
	var id [4]byte
	_, _ = rand.Read(id[:])
	return &store{
		dir:          dir,
		now:          time.Now,
		instance:     fmt.Sprintf("%d-%x", os.Getpid(), id),
		retention:    retentionDays,
		days:         make(map[string]*day),
		dirty:        make(map[string]bool),
		mergedSpills: make(map[string]bool),
	}
}

var (
	current        atomic.Pointer[store]
	runtimeMu      sync.Mutex
	backgroundOnce sync.Once
)

func activeStore() *store { return current.Load() }

// startRuntime points the plugin at cfg's data directory, keeping the store
// across reconfigurations that do not move it.
func startRuntime(cfg settings) error {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if existing := current.Load(); existing != nil && existing.dir == cfg.DataDir {
		existing.setRetention(cfg.RetentionDays)
		// A register after quiesce means the replacement was rolled back.
		existing.resume()
		existing.tick()
	} else {
		if existing != nil {
			existing.close()
		}
		fresh := newStore(cfg.DataDir, cfg.RetentionDays)
		fresh.tick()
		current.Store(fresh)
	}
	prices.configure(cfg.PricingURL, cfg.DataDir)
	customPrices.configure(cfg.DataDir)
	backgroundOnce.Do(func() {
		startBackground(func(ctx context.Context) {
			ticker := time.NewTicker(flushInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if active := activeStore(); active != nil {
						active.tick()
					}
				}
			}
		})
		startBackground(prices.run)
	})
	return nil
}

func (s *store) setRetention(days int) {
	s.mu.Lock()
	s.retention = days
	s.lastPrune = time.Time{}
	s.mu.Unlock()
}

func (s *store) add(event usageEvent) {
	date := event.At.Format(dayLayout)
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.days[date]
	if d == nil {
		d = newDay()
		s.days[date] = d
	}
	if event.Account != "" && event.Identity.Label != "" {
		d.accounts[event.Account] = event.Identity
	}
	d.addRow(rowKey{Hour: event.At.Hour(), Account: event.Account, Provider: event.Provider, Model: event.Model}, event.Counters, event.Session)
	s.dirty[date] = true
}

// tick runs the periodic work: take the lock and load, merge spill files,
// write changed days, and drop days past retention once an hour. Without the
// lock it spills instead.
func (s *store) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded && !s.quiesced {
		s.loadLocked()
	}
	if s.now().Sub(s.lastPrune) >= time.Hour {
		s.pruneLocked()
	}
	if s.loaded {
		s.mergeSpillsLocked()
		s.flushLocked()
	} else {
		s.spillLocked()
	}
}

// quiesce writes everything and gives the directory up for the library
// replacing this one.
func (s *store) quiesce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quiesced = true
	s.releaseLocked()
}

func (s *store) resume() {
	s.mu.Lock()
	s.quiesced = false
	s.mu.Unlock()
}

// close writes pending usage and releases the directory for another
// instance.
func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked()
}

// releaseLocked flushes and unlocks, or spills when this store holds no
// lock. Released, the store keeps nothing in memory: the files have it all.
func (s *store) releaseLocked() {
	if !s.loaded {
		s.spillLocked()
		return
	}
	s.flushLocked()
	if len(s.dirty) > 0 {
		hostLog("warn", fmt.Sprintf("usage store: releasing %s with %d unwritten days", s.dir, len(s.dirty)))
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	s.loaded = false
	s.days = make(map[string]*day)
	s.dirty = make(map[string]bool)
	s.mergedSpills = make(map[string]bool)
}

// spillLocked hands usage collected without the lock to the lock holder.
func (s *store) spillLocked() {
	if len(s.days) == 0 {
		return
	}
	if errMkdir := os.MkdirAll(s.dir, 0o700); errMkdir != nil {
		s.setErrorLocked(fmt.Errorf("create %s: %w", s.dir, errMkdir))
		return
	}
	var failure error
	stamp := s.now().UTC().Format("20060102T150405.000000000")
	for date, d := range s.days {
		name := spillFilePrefix + date + "-" + stamp + "-" + s.instance + dayFileSuffix
		if errWrite := writeDayFileAs(s.dir, name, date, d); errWrite != nil {
			failure = errWrite
			continue
		}
		delete(s.days, date)
		delete(s.dirty, date)
	}
	s.setErrorLocked(failure)
}

// mergeSpillsLocked adds other instances' spill files to the loaded days.
func (s *store) mergeSpillsLocked() {
	entries, errRead := os.ReadDir(s.dir)
	if errRead != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || s.mergedSpills[name] || !strings.HasPrefix(name, spillFilePrefix) || !strings.HasSuffix(name, dayFileSuffix) {
			continue
		}
		date := strings.TrimPrefix(name, spillFilePrefix)
		if len(date) < len(dayLayout) {
			continue
		}
		date = date[:len(dayLayout)]
		if _, errDate := time.Parse(dayLayout, date); errDate != nil {
			continue
		}
		spilled, errLoad := readDayFile(filepath.Join(s.dir, name))
		if errLoad != nil {
			s.moveAsideLocked(name, errLoad)
			continue
		}
		s.mergeLocked(date, spilled)
		s.dirty[date] = true
		s.mergedSpills[name] = true
	}
}

func (s *store) moveAsideLocked(name string, cause error) {
	aside := name + ".corrupt-" + s.now().UTC().Format("20060102T150405")
	_ = os.Rename(filepath.Join(s.dir, name), filepath.Join(s.dir, aside))
	hostLog("warn", fmt.Sprintf("usage store: %s is unreadable (%v), moved to %s", name, cause, aside))
}

func (s *store) setErrorLocked(err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message != "" && message != s.lastError {
		hostLog("warn", "usage store: "+message)
	}
	s.lastError = message
}

func (s *store) loadLocked() {
	if errMkdir := os.MkdirAll(s.dir, 0o700); errMkdir != nil {
		s.setErrorLocked(fmt.Errorf("create %s: %w", s.dir, errMkdir))
		return
	}
	lock, errOpen := os.OpenFile(filepath.Join(s.dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if errOpen != nil {
		s.setErrorLocked(fmt.Errorf("open lock: %w", errOpen))
		return
	}
	if errLock := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errLock != nil {
		_ = lock.Close()
		if !errors.Is(errLock, syscall.EWOULDBLOCK) {
			s.setErrorLocked(fmt.Errorf("lock %s: %w", s.dir, errLock))
		}
		return
	}
	entries, errRead := os.ReadDir(s.dir)
	if errRead != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		s.setErrorLocked(fmt.Errorf("read %s: %w", s.dir, errRead))
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, dayFilePrefix) || !strings.HasSuffix(name, dayFileSuffix) {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, dayFilePrefix), dayFileSuffix)
		if _, errDate := time.Parse(dayLayout, date); errDate != nil {
			continue
		}
		loaded, errLoad := readDayFile(filepath.Join(s.dir, name))
		if errLoad != nil {
			// Keep the unreadable file for inspection; the day restarts empty.
			s.moveAsideLocked(name, errLoad)
			continue
		}
		s.mergeLocked(date, loaded)
	}
	s.lock = lock
	s.loaded = true
	// Loaded files may predate the retention window.
	s.lastPrune = time.Time{}
	s.setErrorLocked(nil)
}

// mergeLocked adds a day read from disk to what was collected in memory
// before the lock was taken.
func (s *store) mergeLocked(date string, loaded *day) {
	d := s.days[date]
	if d == nil {
		s.days[date] = loaded
		return
	}
	for account, info := range loaded.accounts {
		if _, exists := d.accounts[account]; !exists {
			d.accounts[account] = info
		}
	}
	for key, r := range loaded.rows {
		sessions := make([]string, 0, len(r.sessions))
		for session := range r.sessions {
			sessions = append(sessions, session)
		}
		d.addRow(key, r.counters, sessions...)
	}
	s.dirty[date] = true
}

func readDayFile(path string) (*day, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	var file dayFile
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if file.Version != dayFileVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	d := newDay()
	for account, info := range file.Accounts {
		d.accounts[account] = info
	}
	for _, r := range file.Rows {
		if r.Hour < 0 || r.Hour > 23 {
			return nil, fmt.Errorf("row hour %d out of range", r.Hour)
		}
		d.addRow(rowKey{Hour: r.Hour, Account: r.Account, Provider: r.Provider, Model: r.Model}, r.counters, r.Sessions...)
	}
	return d, nil
}

func (s *store) flushLocked() {
	var failure error
	for date := range s.dirty {
		d := s.days[date]
		if d == nil {
			delete(s.dirty, date)
			continue
		}
		if errWrite := writeDayFileAs(s.dir, dayFilePrefix+date+dayFileSuffix, date, d); errWrite != nil {
			failure = errWrite
			continue
		}
		delete(s.dirty, date)
	}
	s.setErrorLocked(failure)
	// Merged spill files go once their usage is in the day files; until then
	// mergedSpills keeps them from being merged twice.
	if len(s.dirty) == 0 {
		for name := range s.mergedSpills {
			if errRemove := os.Remove(filepath.Join(s.dir, name)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				s.setErrorLocked(fmt.Errorf("remove %s: %w", name, errRemove))
				continue
			}
			delete(s.mergedSpills, name)
		}
	}
}

func writeDayFileAs(dir, name, date string, d *day) error {
	file := dayFile{Version: dayFileVersion, Date: date, Accounts: d.accounts, Rows: make([]rowFile, 0, len(d.rows))}
	for key, r := range d.rows {
		sessions := make([]string, 0, len(r.sessions))
		for session := range r.sessions {
			sessions = append(sessions, session)
		}
		sort.Strings(sessions)
		file.Rows = append(file.Rows, rowFile{Hour: key.Hour, Account: key.Account, Provider: key.Provider, Model: key.Model, counters: r.counters, Sessions: sessions})
	}
	sort.Slice(file.Rows, func(i, j int) bool {
		a, b := file.Rows[i], file.Rows[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	raw, errMarshal := json.Marshal(file)
	if errMarshal != nil {
		return fmt.Errorf("encode %s: %w", date, errMarshal)
	}
	return writeFileAtomic(filepath.Join(dir, name), raw)
}

// writeFileAtomic replaces path so that a crash leaves either the old or the
// new content, never a torn file.
func writeFileAtomic(path string, raw []byte) error {
	tmp, errCreate := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if errCreate != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), errCreate)
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(raw)
	if errWrite == nil {
		errWrite = tmp.Sync()
	}
	if errClose := tmp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Chmod(tmpName, 0o600)
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpName, path)
	}
	if errWrite != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", filepath.Base(path), errWrite)
	}
	return nil
}

// pruneLocked drops days older than the retention window. It keeps one extra
// UTC day so a local-time day at the window's start stays complete.
func (s *store) pruneLocked() {
	s.lastPrune = s.now()
	cutoff := s.now().UTC().AddDate(0, 0, -(s.retention + 1)).Format(dayLayout)
	for date := range s.days {
		if date >= cutoff {
			continue
		}
		delete(s.days, date)
		delete(s.dirty, date)
	}
	if !s.loaded {
		return
	}
	entries, errRead := os.ReadDir(s.dir)
	if errRead != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, dayFilePrefix) || !strings.HasSuffix(name, dayFileSuffix) {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, dayFilePrefix), dayFileSuffix)
		if _, errDate := time.Parse(dayLayout, date); errDate != nil || date >= cutoff {
			continue
		}
		if errRemove := os.Remove(filepath.Join(s.dir, name)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			s.setErrorLocked(fmt.Errorf("remove %s: %w", name, errRemove))
		}
	}
}

// storedRow is one row with its hour as an absolute time, handed to report
// code outside the store lock.
type storedRow struct {
	At       time.Time
	Key      rowKey
	Counters counters
	Sessions []string
}

// snapshot copies the rows in [from, to) and the latest saved name of every
// account.
func (s *store) snapshot(from, to time.Time) ([]storedRow, map[string]accountInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dates := make([]string, 0, len(s.days))
	for date := range s.days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	accounts := make(map[string]accountInfo)
	var rows []storedRow
	for _, date := range dates {
		d := s.days[date]
		for account, info := range d.accounts {
			accounts[account] = info
		}
		start, errDate := time.Parse(dayLayout, date)
		if errDate != nil {
			continue
		}
		for key, r := range d.rows {
			at := start.Add(time.Duration(key.Hour) * time.Hour)
			if at.Before(from) || !at.Before(to) {
				continue
			}
			sessions := make([]string, 0, len(r.sessions))
			for session := range r.sessions {
				sessions = append(sessions, session)
			}
			rows = append(rows, storedRow{At: at, Key: key, Counters: r.counters, Sessions: sessions})
		}
	}
	return rows, accounts
}

type storeStatus struct {
	DataDir       string `json:"data_dir"`
	RetentionDays int    `json:"retention_days"`
	Persisting    bool   `json:"persisting"`
	Error         string `json:"error,omitempty"`
}

func (s *store) status() storeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return storeStatus{DataDir: s.dir, RetentionDays: s.retention, Persisting: s.loaded, Error: s.lastError}
}
