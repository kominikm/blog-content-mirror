package blogmirror

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

const (
	initialRetryDelay = time.Second
	maxRetryDelay     = 10 * time.Minute
	retryMultiplier   = 3
)

type snapshotLoader func(context.Context) ([]Post, error)

// cache owns snapshot storage and refresh lifecycle for a mirror.
type cache struct {
	snapshot        []Post
	loadSnapshot    snapshotLoader
	refreshInterval time.Duration
	mutex           sync.RWMutex
	once            sync.Once
	started         bool
	loaded          chan struct{}
	done            chan struct{}
	logger          *slog.Logger
}

func newCache(loadSnapshot snapshotLoader, refreshInterval time.Duration, logger *slog.Logger) (*cache, error) {
	if loadSnapshot == nil {
		return nil, fmt.Errorf("blog mirror snapshot loader is nil")
	}
	if refreshInterval <= 0 {
		return nil, fmt.Errorf("blog mirror refresh interval must be positive")
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &cache{
		loadSnapshot:    loadSnapshot,
		refreshInterval: refreshInterval,
		logger:          logger,
		loaded:          make(chan struct{}),
		done:            make(chan struct{}),
	}, nil
}

func (c *cache) start(ctx context.Context) {
	if !c.shouldStart() {
		return
	}

	go func() {
		defer close(c.done)

		ticker := time.NewTicker(c.refreshInterval)
		defer ticker.Stop()

		for {
			c.loadWithRetry(ctx)

			select {
			case <-ticker.C:
				// Continue with the next refresh
			case <-ctx.Done():
				c.logger.Info("blog mirror received shutdown signal")
				return
			}
		}
	}()
}

func (c *cache) shouldStart() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.started {
		return false
	}

	c.started = true
	return true
}

func (c *cache) wait() {
	<-c.done
}

func (c *cache) posts(ctx context.Context) ([]Post, error) {
	select {
	case <-c.loaded:
		// Continue with the cached snapshot
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return slices.Clone(c.snapshot), nil
}

func (c *cache) postByPath(ctx context.Context, path string) (Post, error) {
	select {
	case <-c.loaded:
		// Continue with the cached snapshot
	case <-ctx.Done():
		return Post{}, ctx.Err()
	}

	c.mutex.RLock()
	defer c.mutex.RUnlock()

	for _, post := range c.snapshot {
		if post.Path == path {
			return post, nil
		}
	}
	return Post{}, ErrPostNotFound
}

func (c *cache) loadWithRetry(ctx context.Context) {
	delay := initialRetryDelay

	for {
		posts, err := c.loadSnapshot(ctx)
		if err != nil {
			c.logger.Warn("blog mirror load failed, retrying...", "error", err, "delay", delay)

			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
				delay *= retryMultiplier
				if delay > maxRetryDelay {
					delay = maxRetryDelay
				}
			}
			continue
		}

		c.mutex.Lock()
		c.snapshot = posts
		c.mutex.Unlock()

		c.once.Do(func() {
			close(c.loaded)
		})

		c.logger.Info("blog mirror loaded successfully")
		return
	}
}
