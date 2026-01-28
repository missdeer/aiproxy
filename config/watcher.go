package config

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Manager manages configuration with hot-reload support
type Manager struct {
	path      string
	config    *Config
	mu        sync.RWMutex
	listeners []func(*Config)
	watchMu   sync.Mutex
	watcher   *fsnotify.Watcher
	done      chan struct{}
}

// NewManager creates a new configuration manager
func NewManager(path string) (*Manager, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		path:   path,
		config: cfg,
	}

	return m, nil
}

// Get returns the current configuration (thread-safe)
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// OnReload registers a callback that will be called when config is reloaded
func (m *Manager) OnReload(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, fn)
}

// StartWatching starts watching the config file for changes
func (m *Manager) StartWatching() error {
	m.watchMu.Lock()
	defer m.watchMu.Unlock()

	if m.watcher != nil {
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Watch the directory containing the config file to handle editors
	// that write to a temp file and rename (atomic write)
	dir := filepath.Dir(m.path)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return err
	}

	done := make(chan struct{})
	m.watcher = watcher
	m.done = done
	go m.watchLoop(watcher, done)

	log.Printf("[CONFIG] Watching for changes: %s", m.path)
	return nil
}

// StopWatching stops the file watcher
func (m *Manager) StopWatching() {
	m.watchMu.Lock()
	watcher := m.watcher
	done := m.done
	m.watcher = nil
	m.done = nil
	m.watchMu.Unlock()

	if watcher == nil {
		return
	}

	close(done)
	_ = watcher.Close()
}

func (m *Manager) watchLoop(watcher *fsnotify.Watcher, done <-chan struct{}) {
	debounceDelay := 500 * time.Millisecond
	configName := filepath.Base(m.path)

	var debounceTimer *time.Timer
	var debounceC <-chan time.Time

	stopTimer := func() {
		if debounceTimer == nil {
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceC = nil
	}
	resetTimer := func() {
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(debounceDelay)
		} else {
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer.Reset(debounceDelay)
		}
		debounceC = debounceTimer.C
	}

	for {
		select {
		case <-done:
			stopTimer()
			return

		case <-debounceC:
			debounceC = nil
			m.reload()

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// Only react to changes to our config file
			if filepath.Base(event.Name) != configName {
				continue
			}

			// React to Write, Create, and Rename events
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}

			resetTimer()

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[CONFIG] Watcher error: %v", err)
		}
	}
}

func (m *Manager) reload() {
	log.Printf("[CONFIG] Detected change, reloading...")

	newCfg, err := Load(m.path)
	if err != nil {
		log.Printf("[CONFIG] Failed to reload config: %v", err)
		return
	}

	m.mu.Lock()
	oldCfg := m.config
	m.config = newCfg
	listeners := make([]func(*Config), len(m.listeners))
	copy(listeners, m.listeners)
	m.mu.Unlock()

	// Log changes
	log.Printf("[CONFIG] Reloaded successfully: %d upstreams (was %d)",
		len(newCfg.Upstreams), len(oldCfg.Upstreams))
	for _, u := range newCfg.Upstreams {
		status := "enabled"
		if !u.IsEnabled() {
			status = "disabled"
		}
		log.Printf("[CONFIG]   - %s (weight: %d, %s)", u.Name, u.Weight, status)
	}

	// Notify listeners
	for _, fn := range listeners {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[CONFIG] Reload listener panic: %v", r)
				}
			}()
			fn(newCfg)
		}()
	}
}
