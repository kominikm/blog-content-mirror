package blogmirror

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

//go:embed testdata/initial-posts.json
var initialPostsResponse string

//go:embed testdata/refreshed-posts.json
var refreshedPostsResponse string

func TestOptionsConfigureConfig(t *testing.T) {
	t.Parallel()

	client := http.Client{Timeout: time.Second}
	tests := []struct {
		name   string
		option Option
		want   config
	}{
		{
			name:   "HTTP client",
			option: WithHTTPClient(&client),
			want:   config{client: &client},
		},
		{
			name:   "refresh interval",
			option: WithRefreshInterval(time.Minute),
			want:   config{refreshInterval: time.Minute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := config{}
			tt.option(&got)
			if got != tt.want {
				t.Fatalf("config = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMirrorE2E(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const apiHost = "api.example.com"
		const mediaHost = "media.example.com"

		imageBytes := []byte("image")
		apiResponses := []string{initialPostsResponse, refreshedPostsResponse}
		apiRequests := 0
		serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Host {
			case mediaHost:
				if r.URL.Path != "/wp-content/uploads/2019/09/001.jpg" {
					t.Errorf("unexpected media request path %q", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "image/jpeg")
				if _, err := w.Write(imageBytes); err != nil {
					t.Errorf("write image response: %v", err)
				}
				return
			case apiHost:
				if r.URL.Path != "/posts" {
					t.Errorf("unexpected API request path %q", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if apiRequests == len(apiResponses) {
					t.Errorf("unexpected API request %d", apiRequests+1)
					http.Error(w, "unexpected API request", http.StatusInternalServerError)
					return
				}
				response := apiResponses[apiRequests]
				apiRequests++
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, response); err != nil {
					t.Errorf("write API response: %v", err)
				}
				return
			default:
				t.Errorf("unexpected request host %q", r.Host)
				http.NotFound(w, r)
				return
			}
		})
		server := httptest.NewTestServer(t, serverHandler)

		imageDir := t.TempDir()
		mirror, err := New(
			"http://"+apiHost,
			"http://"+mediaHost+"/wp-content/uploads/",
			imageDir,
			"/img/blog",
			WithHTTPClient(server.Client()),
			WithLogger(slog.New(slog.DiscardHandler)),
			WithRefreshInterval(time.Hour),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			mirror.Wait()
		}()
		mirror.Start(ctx)

		posts, err := mirror.Posts(t.Context())
		if err != nil {
			t.Fatalf("Posts: %v", err)
		}
		if len(posts) != 1 {
			t.Fatalf("len(posts) = %d, want 1", len(posts))
		}

		post, err := mirror.Post(t.Context(), "cache-demo")
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if post.ID != 15 || post.Path != "cache-demo" || post.Headline != "Cache & Demo" {
			t.Fatalf("post = %#v", post)
		}
		if !post.Added.Equal(time.Date(2019, 9, 20, 15, 16, 26, 0, time.UTC)) {
			t.Fatalf("Added = %v", post.Added)
		}
		if post.Excerpt != "Excerpt & summary" {
			t.Fatalf("Excerpt = %q, want %q", post.Excerpt, "Excerpt & summary")
		}
		if post.ImgName != "2019/09/001.jpg" {
			t.Fatalf("ImgName = %q, want %q", post.ImgName, "2019/09/001.jpg")
		}
		if !strings.Contains(post.Text, `<p class="wp-block-paragraph">Cached body</p>`) {
			t.Fatalf("Text does not contain body: %s", post.Text)
		}
		if !strings.Contains(post.Text, `src="/img/blog/2019/09/001.jpg"`) {
			t.Fatalf("Text does not contain rewritten inline image: %s", post.Text)
		}

		path := filepath.Join(imageDir, "2019/09/001.jpg")
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("stat mirrored image %s: %v", path, err)
		}
		mirroredImage, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read mirrored image %s: %v", path, readErr)
		}
		if !bytes.Equal(mirroredImage, imageBytes) {
			t.Fatalf("mirrored image = %q, want %q", mirroredImage, imageBytes)
		}

		synctest.Sleep(time.Hour)

		posts, err = mirror.Posts(t.Context())
		if err != nil {
			t.Fatalf("refreshed posts: %v", err)
		}
		if len(posts) != 1 || posts[0].Path != "refreshed" {
			t.Fatalf("refreshed posts = %#v", posts)
		}
		if apiRequests != 2 {
			t.Fatalf("API requests = %d, want 2", apiRequests)
		}
	})
}
