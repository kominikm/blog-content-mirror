package blogmirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// ErrPostNotFound reports that a mirrored post does not exist.
var ErrPostNotFound = errors.New("post not found")

// Mirror keeps a refreshed snapshot of blog posts and mirrored media.
type Mirror struct {
	cache *cache
}

// Post is a blog post after parsing, cleanup, media mirroring, and URL rewriting.
type Post struct {
	ID       int
	Added    time.Time
	Headline string
	Path     string
	Text     string
	Excerpt  string
	ImgName  string
}

// Option configures an optional blog mirror dependency or setting.
type Option func(*config)

// WithLogger sets the logger used for mirror activity.
func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		cfg.logger = logger
	}
}

// WithHTTPClient sets the client used to fetch posts and images.
func WithHTTPClient(client *http.Client) Option {
	return func(cfg *config) {
		cfg.client = client
	}
}

// WithRefreshInterval sets the delay between successful mirror refreshes.
func WithRefreshInterval(refreshInterval time.Duration) Option {
	return func(cfg *config) {
		cfg.refreshInterval = refreshInterval
	}
}

// New creates a blog mirror for posts fetched from apiURL and images under uploadURL.
func New(apiURL, uploadURL, imageDir, publicPath string, options ...Option) (Mirror, error) {
	var mirror Mirror

	cfg := config{
		refreshInterval: defaultRefreshInterval,
	}
	for _, option := range options {
		option(&cfg)
	}

	logger := cfg.logger
	if logger == nil {
		logger = slog.Default()
	}

	client := cfg.client
	if client == nil {
		client = http.DefaultClient
	}

	postLoader, err := newLoader(client, apiURL, uploadURL, imageDir, publicPath, logger)
	if err != nil {
		return mirror, err
	}

	c, err := newCache(postLoader.load, cfg.refreshInterval, logger)
	if err != nil {
		return mirror, fmt.Errorf("create blog mirror cache: %w", err)
	}
	mirror.cache = c
	return mirror, nil
}

func (m Mirror) Start(ctx context.Context) {
	m.cache.start(ctx)
}

func (m Mirror) Wait() {
	m.cache.wait()
}

// Posts returns a copy of the current mirrored post snapshot.
func (m Mirror) Posts(ctx context.Context) ([]Post, error) {
	return m.cache.posts(ctx)
}

func (m Mirror) Post(ctx context.Context, path string) (Post, error) {
	return m.cache.postByPath(ctx, path)
}

const defaultRefreshInterval = time.Hour

type config struct {
	logger          *slog.Logger
	client          *http.Client
	refreshInterval time.Duration
}
