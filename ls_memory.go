package webdav

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-webdav/internal"
)

// MemoryLockSystem is an in-memory implementation of the LockSystem interface using sync.Map.
type MemoryLockSystem struct {
	locks     sync.Map // token -> *activeLock
	pathLocks sync.Map // path -> []string (tokens)
}

var _ LockSystem = (*MemoryLockSystem)(nil)

// NewMemoryLockSystem creates a new in-memory lock system.
func NewMemoryLockSystem() *MemoryLockSystem {
	return &MemoryLockSystem{}
}

type activeLock struct {
	token     string
	root      string
	depth     internal.Depth
	timeout   time.Time
	exclusive bool
}

// generateLockToken generates a unique lock token
func (m *MemoryLockSystem) generateLockToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "opaquelocktoken:" + hex.EncodeToString(bytes), nil
}

// cleanupExpiredLocks removes expired locks
func (m *MemoryLockSystem) cleanupExpiredLocks() {
	now := time.Now()
	m.locks.Range(func(key, value interface{}) bool {
		token := key.(string)
		lock := value.(*activeLock)
		if now.After(lock.timeout) {
			m.removeLockFromPath(lock.root, token)
			m.locks.Delete(token)
		}
		return true
	})
}

// removeLockFromPath removes a lock token from a path's lock list
func (m *MemoryLockSystem) removeLockFromPath(path, token string) {
	if value, ok := m.pathLocks.Load(path); ok {
		tokens := value.([]string)
		for i, t := range tokens {
			if t == token {
				newTokens := append(tokens[:i], tokens[i+1:]...)
				if len(newTokens) == 0 {
					m.pathLocks.Delete(path)
				} else {
					m.pathLocks.Store(path, newTokens)
				}
				break
			}
		}
	}
}

// HasConflictingLock checks if there are conflicting locks for a path
func (m *MemoryLockSystem) HasConflictingLock(path string, depth internal.Depth, excludeToken string) bool {
	// Check direct locks on this path
	if value, exists := m.pathLocks.Load(path); exists {
		tokens := value.([]string)
		for _, token := range tokens {
			if token != excludeToken {
				if lockValue, exists := m.locks.Load(token); exists {
					lock := lockValue.(*activeLock)
					if lock.exclusive {
						return true
					}
				}
			}
		}
	}

	// Check for parent collection locks with depth infinity
	conflict := false
	m.pathLocks.Range(func(key, value interface{}) bool {
		lockPath := key.(string)
		if strings.HasPrefix(path, lockPath+"/") || (lockPath == "/" && path != "/") {
			tokens := value.([]string)
			for _, token := range tokens {
				if token != excludeToken {
					if lockValue, exists := m.locks.Load(token); exists {
						lock := lockValue.(*activeLock)
						if lock.exclusive && lock.depth == internal.DepthInfinity {
							conflict = true
							return false // Found conflict, stop iteration
						}
					}
				}
			}
		}
		return true
	})
	if conflict {
		return true
	}

	// If this is a depth infinity lock, check for conflicts in child paths
	if depth == internal.DepthInfinity {
		m.pathLocks.Range(func(key, value interface{}) bool {
			lockPath := key.(string)
			if strings.HasPrefix(lockPath, path+"/") || (path == "/" && lockPath != "/") {
				tokens := value.([]string)
				for _, token := range tokens {
					if token != excludeToken {
						if lockValue, exists := m.locks.Load(token); exists {
							lock := lockValue.(*activeLock)
							if lock.exclusive {
								conflict = true
								return false // Found conflict, stop iteration
							}
						}
					}
				}
			}
			return true
		})
		if conflict {
			return true
		}
	}

	return false
}

// Lock attempts to create a new lock for the given path with specified parameters
func (m *MemoryLockSystem) Lock(path string, depth internal.Depth, timeout time.Duration, refreshToken string) (lock *internal.Lock, created bool, err error) {
	// Clean up expired locks first
	m.cleanupExpiredLocks()

	// Handle lock refresh
	if refreshToken != "" {
		if lockValue, exists := m.locks.Load(refreshToken); exists {
			existingLock := lockValue.(*activeLock)
			// Update timeout
			if timeout > 0 {
				existingLock.timeout = time.Now().Add(timeout)
			} else {
				existingLock.timeout = time.Now().Add(24 * time.Hour) // Default timeout
			}
			m.locks.Store(refreshToken, existingLock)

			return &internal.Lock{
				Href:    refreshToken,
				Root:    existingLock.root,
				Timeout: timeout,
			}, false, nil
		}
		return nil, false, internal.HTTPErrorf(http.StatusPreconditionFailed, "webdav: lock token not found")
	}

	// Check for conflicting locks
	if m.HasConflictingLock(path, depth, "") {
		return nil, false, internal.HTTPErrorf(http.StatusLocked, "webdav: resource is locked")
	}

	// Generate new lock token
	token, err := m.generateLockToken()
	if err != nil {
		return nil, false, fmt.Errorf("webdav: failed to generate lock token: %v", err)
	}

	// Set timeout
	var lockTimeout time.Time
	if timeout > 0 {
		lockTimeout = time.Now().Add(timeout)
	} else {
		lockTimeout = time.Now().Add(24 * time.Hour) // Default 24 hour timeout
	}

	// Create the lock
	newLock := &activeLock{
		token:     token,
		root:      path,
		depth:     depth,
		timeout:   lockTimeout,
		exclusive: true, // Only exclusive locks are supported for now
	}

	// Store the lock
	m.locks.Store(token, newLock)

	// Add token to path locks
	if value, exists := m.pathLocks.Load(path); exists {
		tokens := value.([]string)
		tokens = append(tokens, token)
		m.pathLocks.Store(path, tokens)
	} else {
		m.pathLocks.Store(path, []string{token})
	}

	return &internal.Lock{
		Href:    token,
		Root:    path,
		Timeout: timeout,
	}, true, nil
}

// Unlock removes the lock identified by the given token
func (m *MemoryLockSystem) Unlock(tokenHref string) error {
	// Clean up expired locks first
	m.cleanupExpiredLocks()

	// Find the lock
	lockValue, exists := m.locks.Load(tokenHref)
	if !exists {
		return internal.HTTPErrorf(http.StatusConflict, "webdav: lock token not found")
	}

	lock := lockValue.(*activeLock)

	// Remove the lock from path tracking
	m.removeLockFromPath(lock.root, tokenHref)

	// Remove the lock from the main storage
	m.locks.Delete(tokenHref)

	return nil
}
