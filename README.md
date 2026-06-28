# Blog Content Mirror

Originally built for my blog at [kmnk.cz](https://kmnk.cz), this project fetches posts from the [WordPress REST API](https://developer.wordpress.org/rest-api/), mirrors referenced media locally, rewrites content image URLs, and serves reads from an asynchronously refreshed in-memory snapshot.

## Repository Status And Terms

This repository is published as a portfolio reference, not as a maintained public dependency. No open-source licence is granted, but you are welcome to study this code and create private derivative works from it for learning or evaluation.

## Technical Summary

- clear separation of public API, cache lifecycle, and WordPress API loading concerns
- asynchronous cache startup with first-load readiness and graceful shutdown
- retrying background refreshes that replace snapshots atomically
- a small exported `Mirror` type that coordinates refresh and loading through a narrow API
- local media mirroring and content image URL rewriting
- focussed `Mirror`, `cache`, and `loader` tests plus an end-to-end `Mirror` test with local HTTP test servers

## Design

The package is built around the exported `Mirror` type and two unexported components. The `cache` type owns the in-memory snapshot and refresh lifecycle. The `loader` type owns WordPress fetching, post mapping, media mirroring, and content URL rewriting.

The runtime flow is:

1. `New` validates its required configuration and wires `loader` into `cache`.
2. `Start(ctx)` launches the cache refresh loop.
3. On each load attempt, `cache` asks `loader` for a fresh post snapshot.
4. `loader` fetches WordPress JSON, maps it into exported posts, mirrors media, and rewrites content image URLs.
5. A successful load atomically replaces the cached snapshot read by `Posts(ctx)` and `Post(ctx, path)`.

The source files mirror those responsibilities:

- `mirror.go`: public API, dependency wiring, lifecycle methods
- `cache.go`: snapshot storage, first-load readiness, retry, background refresh, shutdown
- `loader.go`: WordPress REST API fetch, JSON decoding, post mapping, media mirroring, content image rewriting

The public boundary stays narrow: callers use `Mirror`, `Post`, and functional options, and handle any mapping to types outside this package.

## Public API

`Mirror` is the exported entry point. Start it with the application context and wait for it during shutdown:

```go
func run(ctx context.Context) error {
	apiURL := "https://public-api.wordpress.com/wp/v2/sites/example.wordpress.com"
	uploadURL := "https://example.wordpress.com/wp-content/uploads/"
	imageDir := "/usr/share/nginx/html/img/blog"
	publicPath := "/img/blog"

	mirror, err := blogmirror.New(
		apiURL,
		uploadURL,
		imageDir,
		publicPath,
	)
	if err != nil {
		return err
	}

	mirror.Start(ctx)
	defer mirror.Wait()

	<-ctx.Done()
	return nil
}
```

After `Start`, reads can happen from handlers or other application code:

```go
posts, err := mirror.Posts(r.Context())
post, err := mirror.Post(r.Context(), "post-slug")
```

`Posts(ctx)` and `Post(ctx, path)` wait for the first successful load. If `ctx` is cancelled before the snapshot is ready, they return `ctx.Err()`.

`Posts(ctx)` returns a copy of the cached slice to avoid mutating the stored snapshot.

`Post(ctx, path)` returns `ErrPostNotFound` when the snapshot is ready but no post matches the path.

## Configuration Expectations

`New` validates the required arguments before constructing a `Mirror`:

- `apiURL` must be an absolute `http` or `https` URL for the WordPress API.
- `uploadURL` must be an absolute `http` or `https` URL and should match the prefix of media URLs referenced by WordPress posts, usually ending with `/wp-content/uploads/`.
- `imageDir` must be an absolute path to an existing writable directory for the local mirrored copy. The root directory is not created by the package; per-image subdirectories are created as needed.
- `publicPath` must be non-empty and should be the public URL path that serves files from `imageDir`.
- `WithHTTPClient` is optional and defaults to `http.DefaultClient`.
- `WithLogger` is optional and defaults to `slog.Default()`.
- `WithRefreshInterval` is optional, defaults to one hour, and requires a positive duration when provided.

## Refresh Behaviour

`Start(ctx)` runs a background refresh loop:

- the first successful load unlocks readers
- failed loads retry with bounded backoff
- successful refreshes replace the cached snapshot atomically
- cancelling `ctx` stops the loop
- `Wait()` blocks until the loop exits

The retry policy is package-internal.

## Loading And Data Mapping

On each load attempt, `cache` asks `loader` for a fresh post snapshot. `loader` fetches WordPress posts, decodes `apiPost` values, maps them to `Post`, mirrors media, and returns the new snapshot.

There are two post shapes inside the package:

- `apiPost`: unexported struct matching the WordPress JSON response
- `Post`: exported mirrored post after parsing, cleanup, media mirroring, and URL rewriting

## Media Behaviour

Featured images and inline content images are downloaded under `ImageDir` using the WordPress upload path suffix. Rewritten content image URLs use `PublicPath`.

Existing local files are reused and not downloaded again.

Partial media failures do not fail the post load: failed featured image downloads leave `Post.ImgName` empty, and failed inline image downloads remove that image tag from the rewritten content. This keeps the blog available even when individual media files are missing or temporarily unavailable.

## Quality Checks

Run all local checks, including formatting, static analysis, ordinary tests, and race detection:

```sh
make check
```

Run only the test suite with:

```sh
make test
```

Generate a terminal coverage summary with:

```sh
make coverage
```

Or generate an HTML report, which also produces the coverage summary:

```sh
make coverage-html
```
