// Package keylock gives a lock per key, such as an image digest, kept only
// while someone holds or waits for it.
package keylock

import "sync"

// Map is a set of locks by key. The zero value is ready to use.
type Map struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	sync.Mutex
	users int
}

// Lock takes the lock of key and returns its unlock.
func (m *Map) Lock(key string) (unlock func()) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*entry{}
	}
	e := m.locks[key]
	if e == nil {
		e = &entry{}
		m.locks[key] = e
	}
	e.users++
	m.mu.Unlock()
	e.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.Unlock()
			m.mu.Lock()
			defer m.mu.Unlock()
			if e.users--; e.users == 0 {
				delete(m.locks, key)
			}
		})
	}
}

// Len returns how many keys have a lock held or waited for.
func (m *Map) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}
