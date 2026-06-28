package blogmirror

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoaderRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	type loaderConfig struct {
		apiURL     string
		uploadURL  string
		imageDir   string
		publicPath string
	}

	imageDir := t.TempDir()
	file, createErr := os.CreateTemp(t.TempDir(), "image-dir-file")
	if createErr != nil {
		t.Fatalf("create temp file: %v", createErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatalf("close temp file: %v", closeErr)
	}

	tests := []struct {
		name string
		cfg  loaderConfig
	}{
		{
			name: "empty API URL",
			cfg: loaderConfig{
				uploadURL:  "http://example.test/uploads/",
				imageDir:   imageDir,
				publicPath: "/img/blog",
			},
		},
		{
			name: "invalid upload URL",
			cfg: loaderConfig{
				apiURL:     "http://example.test/wp-json/wp/v2",
				uploadURL:  "ftp://example.test/uploads/",
				imageDir:   imageDir,
				publicPath: "/img/blog",
			},
		},
		{
			name: "relative image directory",
			cfg: loaderConfig{
				apiURL:     "http://example.test/wp-json/wp/v2",
				uploadURL:  "http://example.test/uploads/",
				imageDir:   "relative",
				publicPath: "/img/blog",
			},
		},
		{
			name: "file image directory",
			cfg: loaderConfig{
				apiURL:     "http://example.test/wp-json/wp/v2",
				uploadURL:  "http://example.test/uploads/",
				imageDir:   file.Name(),
				publicPath: "/img/blog",
			},
		},
		{
			name: "empty public path",
			cfg: loaderConfig{
				apiURL:    "http://example.test/wp-json/wp/v2",
				uploadURL: "http://example.test/uploads/",
				imageDir:  imageDir,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newLoader(
				http.DefaultClient,
				tt.cfg.apiURL,
				tt.cfg.uploadURL,
				tt.cfg.imageDir,
				tt.cfg.publicPath,
				slog.New(slog.DiscardHandler),
			)
			if err == nil {
				t.Fatal("newLoader error is nil")
			}
		})
	}
}

func TestNewLoaderNormalizesURLsAndSetsFields(t *testing.T) {
	t.Parallel()

	l, err := newLoader(
		http.DefaultClient,
		"http://example.test/wp-json/wp/v2?per_page=20&_fields=bad",
		"http://example.test/wp-content/uploads/",
		t.TempDir(),
		"/img/blog",
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatalf("newLoader: %v", err)
	}

	q := l.apiURL.Query()
	if got := l.apiURL.Path; got != "/wp-json/wp/v2/posts" {
		t.Fatalf("API path = %q, want /wp-json/wp/v2/posts", got)
	}
	const wantFields = "id,title,slug,date,excerpt,content,jetpack_featured_media_url"
	if got := q.Get("_fields"); got != wantFields {
		t.Fatalf("_fields = %q, want %q", got, wantFields)
	}
	if got := q.Get("per_page"); got != "20" {
		t.Fatalf("per_page = %q, want 20", got)
	}
	if got := l.uploadURL.Path; got != "/wp-content/uploads/" {
		t.Fatalf("upload path = %q, want /wp-content/uploads/", got)
	}
}

func TestLoaderLoadReturnsErrorForInvalidAPIResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "non-OK status",
			status: http.StatusBadGateway,
			body:   `[]`,
		},
		{
			name:   "invalid JSON",
			status: http.StatusOK,
			body:   `not json`,
		},
		{
			name:   "invalid date",
			status: http.StatusOK,
			body:   `[{"date":"2026-06-22"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Errorf("write API response: %v", err)
				}
			}))
			t.Cleanup(apiServer.Close)

			l := newTestLoader(t, apiServer.URL, apiServer.URL+"/wp-content/uploads/", t.TempDir())
			if _, err := l.load(t.Context()); err == nil {
				t.Fatal("load error is nil")
			}
		})
	}
}

func TestLoaderLoadSkipsAlreadyDownloadedImage(t *testing.T) {
	t.Parallel()

	imageDir := t.TempDir()
	localImage := filepath.Join(imageDir, "2026/06/featured.jpg")
	if err := os.MkdirAll(filepath.Dir(localImage), 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(localImage, []byte("existing"), 0o644); err != nil {
		t.Fatalf("write existing image: %v", err)
	}

	imageRequests := 0
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wp-content/uploads/2026/06/featured.jpg" {
			http.NotFound(w, r)
			return
		}
		imageRequests++
		if _, err := io.WriteString(w, "new image"); err != nil {
			t.Errorf("write image response: %v", err)
		}
	}))
	t.Cleanup(mediaServer.Close)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `[{
			"date": "2026-06-22T10:30:00",
			"jetpack_featured_media_url": "`+mediaServer.URL+`/wp-content/uploads/2026/06/featured.jpg"
		}]`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))
	t.Cleanup(apiServer.Close)

	l := newTestLoader(t, apiServer.URL+"/wp-json/wp/v2", mediaServer.URL+"/wp-content/uploads/", imageDir)
	posts, err := l.load(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(posts) != 1 || posts[0].ImgName != "2026/06/featured.jpg" {
		t.Fatalf("posts = %#v", posts)
	}
	if imageRequests != 0 {
		t.Fatalf("image requests = %d, want 0", imageRequests)
	}
}

func TestLoaderLoadKeepsPostWhenImageDownloadsFail(t *testing.T) {
	t.Parallel()

	mediaServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(mediaServer.Close)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `[{
			"date": "2026-06-22T10:30:00",
			"jetpack_featured_media_url": "`+mediaServer.URL+`/wp-content/uploads/2026/06/missing-featured.jpg",
			"content": {"rendered": "<p>Body</p><img src=\"`+mediaServer.URL+`/wp-content/uploads/2026/06/missing-inline.jpg\">"}
		}]`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))
	t.Cleanup(apiServer.Close)

	l := newTestLoader(t, apiServer.URL+"/wp-json/wp/v2", mediaServer.URL+"/wp-content/uploads/", t.TempDir())
	posts, err := l.load(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("len(posts) = %d, want 1", len(posts))
	}
	if posts[0].ImgName != "" {
		t.Fatalf("ImgName = %q, want empty", posts[0].ImgName)
	}
	if strings.Contains(posts[0].Text, "<img") {
		t.Fatalf("Text still contains failed image tag: %s", posts[0].Text)
	}
	if !strings.Contains(posts[0].Text, "<p>Body</p>") {
		t.Fatalf("Text does not contain body: %s", posts[0].Text)
	}
}

func TestLoaderReplaceContentImagesRewritesSingleQuotedImage(t *testing.T) {
	t.Parallel()

	imageDir := t.TempDir()
	mediaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wp-content/uploads/2026/06/inline.jpg" {
			http.NotFound(w, r)
			return
		}
		if _, err := io.WriteString(w, "image"); err != nil {
			t.Errorf("write image response: %v", err)
			return
		}
	}))
	t.Cleanup(mediaServer.Close)

	l := newTestLoader(t, "http://example.test/wp-json/wp/v2", mediaServer.URL+"/wp-content/uploads/", imageDir)
	content := `<p>Body</p><img class='aligncenter' src='` + mediaServer.URL + `/wp-content/uploads/2026/06/inline.jpg'>`

	got := l.replaceContentImages(t.Context(), content)
	if !strings.Contains(got, `<img class='aligncenter' src='/img/blog/2026/06/inline.jpg'>`) {
		t.Fatalf("rewritten content = %s", got)
	}
	path := filepath.Join(imageDir, "2026/06/inline.jpg")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat mirrored image %s: %v", path, err)
	}
}

func TestResolveUploadImgPath(t *testing.T) {
	t.Parallel()

	l := newTestLoader(t, "http://example.test/wp-json/wp/v2", "http://example.test/wp-content/uploads", t.TempDir())

	tests := []struct {
		name      string
		rawImgURL string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid upload image",
			rawImgURL: "http://example.test/wp-content/uploads/2026/06/photo.jpg",
			want:      "2026/06/photo.jpg",
		},
		{
			name:      "upload root is not an image path",
			rawImgURL: "http://example.test/wp-content/uploads/",
			wantErr:   true,
		},
		{
			name:      "upload path traversal",
			rawImgURL: "http://example.test/wp-content/uploads/../photo.jpg",
			wantErr:   true,
		},
		{
			name:      "outside upload host",
			rawImgURL: "http://other.test/wp-content/uploads/2026/06/photo.jpg",
			wantErr:   true,
		},
		{
			name:      "outside upload scheme",
			rawImgURL: "https://example.test/wp-content/uploads/2026/06/photo.jpg",
			wantErr:   true,
		},
		{
			name:      "outside upload path",
			rawImgURL: "http://example.test/wp-content/other/2026/06/photo.jpg",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := l.resolveUploadImgPath(tt.rawImgURL)
			if tt.wantErr {
				if err == nil {
					t.Fatal("error is nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve upload image path: %v", err)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func newTestLoader(t *testing.T, apiURL, uploadURL, imageDir string) loader {
	t.Helper()

	l, err := newLoader(
		http.DefaultClient,
		apiURL,
		uploadURL,
		imageDir,
		"/img/blog",
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatalf("newLoader: %v", err)
	}
	return l
}
