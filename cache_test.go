package blogmirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

func TestNewCacheRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		loadSnapshot    snapshotLoader
		refreshInterval time.Duration
	}{
		{
			name:            "nil snapshot loader",
			refreshInterval: time.Hour,
		},
		{
			name: "non-positive refresh interval",
			loadSnapshot: func(_ context.Context) ([]Post, error) {
				return nil, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newCache(tt.loadSnapshot, tt.refreshInterval, slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("newCache error is nil")
			}
		})
	}
}

func TestCacheReadsReturnCancelledContextBeforeInitialLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		read func(*cache, context.Context) error
	}{
		{
			name: "posts",
			read: func(c *cache, ctx context.Context) error {
				_, err := c.posts(ctx)
				return err
			},
		},
		{
			name: "post by path",
			read: func(c *cache, ctx context.Context) error {
				_, err := c.postByPath(ctx, "item")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &cache{
				loaded: make(chan struct{}),
				logger: slog.New(slog.DiscardHandler),
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := tt.read(c, ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("read error = %v, want %v", err, context.Canceled)
			}
		})
	}
}

func TestCachePostsReturnsSnapshotCopy(t *testing.T) {
	t.Parallel()

	c := &cache{
		loaded: make(chan struct{}),
		snapshot: []Post{
			{Path: "cached-item", Headline: "Cached item"},
		},
		logger: slog.New(slog.DiscardHandler),
	}
	close(c.loaded)

	posts, err := c.posts(t.Context())
	if err != nil {
		t.Fatalf("posts: %v", err)
	}
	posts[0].Headline = "Mutated"

	posts, err = c.posts(t.Context())
	if err != nil {
		t.Fatalf("posts after mutation: %v", err)
	}
	if posts[0].Headline != "Cached item" {
		t.Fatalf("Headline = %q, want %q", posts[0].Headline, "Cached item")
	}
}

func TestCachePostByPathWaitsForInitialLoad(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c := &cache{
			loaded: make(chan struct{}),
			snapshot: []Post{
				{Path: "ready-item", Headline: "Ready item"},
			},
			logger: slog.New(slog.DiscardHandler),
		}

		done := make(chan error, 1)
		go func() {
			post, err := c.postByPath(t.Context(), "ready-item")
			if err != nil {
				done <- err
				return
			}
			if post.Headline != "Ready item" {
				done <- fmt.Errorf("Headline = %q, want %q", post.Headline, "Ready item")
				return
			}
			done <- nil
		}()

		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("postByPath returned before cache was loaded: %v", err)
		default:
		}

		close(c.loaded)
		if err := <-done; err != nil {
			t.Fatalf("postByPath error: %v", err)
		}
	})
}

func TestCachePostByPathReturnsNotFoundError(t *testing.T) {
	t.Parallel()

	c := &cache{
		loaded: make(chan struct{}),
		snapshot: []Post{
			{Path: "existing"},
		},
		logger: slog.New(slog.DiscardHandler),
	}
	close(c.loaded)

	_, err := c.postByPath(t.Context(), "missing")
	if !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("postByPath error = %v, want %v", err, ErrPostNotFound)
	}
}

func TestCacheLoadWithRetryStopsWhenContextCancelled(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		c, err := newCache(func(_ context.Context) ([]Post, error) {
			attempts++
			return nil, fmt.Errorf("temporary failure")
		}, time.Hour, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("new cache: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			c.loadWithRetry(ctx)
			close(done)
		}()

		synctest.Wait()
		cancel()
		<-done

		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
	})
}

func TestCacheLoadWithRetryRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		requests := 0
		c, err := newCache(func(_ context.Context) ([]Post, error) {
			requests++
			if requests == 1 {
				return nil, fmt.Errorf("temporary failure")
			}
			return []Post{{Path: "retry-item", Headline: "Retry item"}}, nil
		}, time.Hour, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("new cache: %v", err)
		}

		done := make(chan struct{})
		go func() {
			c.loadWithRetry(t.Context())
			close(done)
		}()

		synctest.Wait()
		<-done

		post, err := c.postByPath(t.Context(), "retry-item")
		if err != nil {
			t.Fatalf("post after retry: %v", err)
		}
		if post.Headline != "Retry item" {
			t.Fatalf("Headline = %q, want %q", post.Headline, "Retry item")
		}
		if requests != 2 {
			t.Fatalf("requests = %d, want 2", requests)
		}
	})
}

func TestCacheRefreshReplacesSnapshot(t *testing.T) {
	t.Parallel()

	snapshots := [][]Post{
		{{Path: "first", Headline: "First"}},
		{{Path: "second", Headline: "Second"}},
	}
	loads := 0
	c, err := newCache(func(_ context.Context) ([]Post, error) {
		loads++
		load := min(loads, len(snapshots))
		return snapshots[load-1], nil
	}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	c.loadWithRetry(t.Context())
	if _, err = c.postByPath(t.Context(), "first"); err != nil {
		t.Fatalf("first snapshot post: %v", err)
	}

	c.loadWithRetry(t.Context())
	if _, err = c.postByPath(t.Context(), "first"); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("old snapshot post error = %v, want %v", err, ErrPostNotFound)
	}
	post, err := c.postByPath(t.Context(), "second")
	if err != nil {
		t.Fatalf("second snapshot post: %v", err)
	}
	if post.Headline != "Second" {
		t.Fatalf("Headline = %q, want %q", post.Headline, "Second")
	}
}

func TestCacheStartOnlyStartsOnce(t *testing.T) {
	t.Parallel()

	loads := 0
	c, err := newCache(func(_ context.Context) ([]Post, error) {
		loads++
		return []Post{{Path: "started", Headline: "Started"}}, nil
	}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	c.start(ctx)
	c.start(ctx)

	if _, err = c.posts(t.Context()); err != nil {
		t.Fatalf("posts: %v", err)
	}

	cancel()
	c.wait()

	if loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
}

func TestCacheStartStopsCleanly(t *testing.T) {
	t.Parallel()

	c, err := newCache(func(_ context.Context) ([]Post, error) {
		return []Post{{Path: "started", Headline: "Started"}}, nil
	}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	c.start(ctx)

	if _, err = c.posts(t.Context()); err != nil {
		t.Fatalf("posts: %v", err)
	}

	cancel()
	c.wait()
}
