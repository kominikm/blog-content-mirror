package blogmirror

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	urlpath "path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	imgSrcRegex = regexp.MustCompile(`<img[^>]+src=["']([^"']+)["']`)
	tagRegex    = regexp.MustCompile("<[^>]*>")
)

const apiPostFields = "id,title,slug,date,excerpt,content,jetpack_featured_media_url"

// loader fetches posts from the WordPress REST API and mirrors referenced media before posts enter the cache.
type loader struct {
	logger     *slog.Logger
	client     *http.Client
	apiURL     *url.URL
	uploadURL  *url.URL
	imageDir   string
	publicPath string
}

type apiPost struct {
	ID    int `json:"id"`
	Title struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Slug    string `json:"slug"`
	Date    string `json:"date"`
	Excerpt struct {
		Rendered string `json:"rendered"`
	} `json:"excerpt"`
	FeaturedMediaURL string `json:"jetpack_featured_media_url"`
	Content          struct {
		Rendered string `json:"rendered"`
	} `json:"content"`
}

func newLoader(client *http.Client, rawAPIURL, rawUploadURL, imageDir, publicPath string, logger *slog.Logger) (loader, error) {
	apiURL, err := postsAPIURL(rawAPIURL)
	if err != nil {
		return loader{}, err
	}

	uploadURL, err := normalizeUploadURL(rawUploadURL)
	if err != nil {
		return loader{}, err
	}

	if err = validateDir(imageDir); err != nil {
		return loader{}, err
	}

	if publicPath == "" {
		return loader{}, fmt.Errorf("blog mirror public path is empty")
	}

	return loader{
		logger:     logger,
		client:     client,
		apiURL:     apiURL,
		uploadURL:  uploadURL,
		imageDir:   imageDir,
		publicPath: publicPath,
	}, nil
}

func postsAPIURL(rawURL string) (*url.URL, error) {
	apiURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("APIURL must be an absolute HTTP URL, got %q: %w", rawURL, err)
	}

	if err = validateURL(apiURL); err != nil {
		return nil, fmt.Errorf("validate APIURL: %w", err)
	}

	apiURL.Path = urlpath.Join(apiURL.Path, "posts")
	q := apiURL.Query()
	q.Set("_fields", apiPostFields)
	apiURL.RawQuery = q.Encode()
	return apiURL, nil
}

func normalizeUploadURL(rawURL string) (*url.URL, error) {
	uploadURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("UploadURL must be an absolute HTTP URL, got %q: %w", rawURL, err)
	}

	if err = validateURL(uploadURL); err != nil {
		return nil, fmt.Errorf("validate UploadURL: %w", err)
	}

	if !strings.HasSuffix(uploadURL.Path, "/") {
		uploadURL.Path += "/"
	}

	return uploadURL, nil
}

func validateURL(parsedURL *url.URL) error {
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("URL must be an absolute HTTP URL, got %q", parsedURL.String())
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("URL must be an absolute HTTP URL, got %q", parsedURL.String())
	}
	return nil
}

func validateDir(imageDir string) error {
	if !filepath.IsAbs(imageDir) {
		return fmt.Errorf("imageDir must be an absolute path, got %q", imageDir)
	}

	fi, err := os.Stat(imageDir)
	if err != nil {
		return fmt.Errorf("stat image directory %q: %w", imageDir, err)
	}

	if !fi.IsDir() {
		return fmt.Errorf("image directory %q is not a directory", imageDir)
	}

	tf, err := os.CreateTemp(imageDir, ".write_test_*")
	if err != nil {
		return fmt.Errorf("no write access to image directory %q: %w", imageDir, err)
	}
	defer func() {
		tf.Close()
		os.Remove(tf.Name())
	}()

	return nil
}

func (l loader) load(ctx context.Context) ([]Post, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.apiURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create WordPress posts request: %w", err)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch WordPress posts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch WordPress posts: bad status %s", resp.Status)
	}

	var apiPosts []apiPost
	if err = json.UnmarshalRead(resp.Body, &apiPosts); err != nil {
		return nil, fmt.Errorf("decode WordPress posts response: %w", err)
	}

	posts := make([]Post, 0, len(apiPosts))
	for _, p := range apiPosts {
		post, err := l.toPost(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("map WordPress post %d: %w", p.ID, err)
		}
		posts = append(posts, post)
	}

	return posts, nil
}

func (l loader) toPost(ctx context.Context, p apiPost) (Post, error) {
	added, err := time.Parse("2006-01-02T15:04:05", p.Date)
	if err != nil {
		return Post{}, fmt.Errorf("parse post date %q: %w", p.Date, err)
	}

	var imgName string
	if p.FeaturedMediaURL != "" {
		if imgName, err = l.downloadImage(ctx, p.FeaturedMediaURL); err != nil {
			l.logger.Error("failed to download featured image", "url", p.FeaturedMediaURL, "error", err)
		}
	}

	return Post{
		ID:       p.ID,
		Headline: html.UnescapeString(p.Title.Rendered),
		Path:     p.Slug,
		Added:    added,
		Text:     l.replaceContentImages(ctx, p.Content.Rendered),
		Excerpt:  stripTags(html.UnescapeString(strings.TrimSpace(p.Excerpt.Rendered))),
		ImgName:  imgName,
	}, nil
}

// replaceContentImages mirrors image sources to local files and rewrites src attributes to public paths.
// Images that cannot be mirrored are removed from the content.
func (l loader) replaceContentImages(ctx context.Context, content string) string {
	downloaded := make(map[string]string) // avoid downloading the same image more than once within one post body

	return imgSrcRegex.ReplaceAllStringFunc(content, func(imgTag string) string {
		matches := imgSrcRegex.FindStringSubmatch(imgTag)
		if len(matches) < 2 {
			return imgTag
		}

		originalURL := matches[1]

		if publicImgPath, ok := downloaded[originalURL]; ok {
			return strings.Replace(imgTag, originalURL, publicImgPath, 1)
		}

		imgName, err := l.downloadImage(ctx, originalURL)
		if err != nil {
			l.logger.Warn("failed to download content image", "url", originalURL, "error", err)
			return ""
		}
		publicImgPath := urlpath.Join(l.publicPath, imgName)
		downloaded[originalURL] = publicImgPath
		return strings.Replace(imgTag, originalURL, publicImgPath, 1)
	})
}

func stripTags(s string) string {
	return tagRegex.ReplaceAllString(s, "")
}

func (l loader) downloadImage(ctx context.Context, rawImgURL string) (string, error) {
	imgPath, err := l.resolveUploadImgPath(rawImgURL)
	if err != nil {
		return "", err
	}

	relativePath, err := filepath.Localize(imgPath)
	if err != nil {
		return "", fmt.Errorf("localize resolved image path %q: %w", imgPath, err)
	}
	imgFile := filepath.Join(l.imageDir, relativePath)

	if _, err = os.Stat(imgFile); err == nil {
		return imgPath, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat image file %q: %w", imgFile, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawImgURL, nil)
	if err != nil {
		return "", fmt.Errorf("create image request %q: %w", rawImgURL, err)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download image %q: %w", rawImgURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download image %q: bad status %s", rawImgURL, resp.Status)
	}

	dir := filepath.Dir(imgFile)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return "", fmt.Errorf("create image directory %q: %w", dir, err)
	}

	out, err := os.Create(imgFile)
	if err != nil {
		return "", fmt.Errorf("create image file %q: %w", imgFile, err)
	}

	_, err = io.Copy(out, resp.Body)
	closeErr := out.Close()
	if err != nil {
		return "", fmt.Errorf("write image file %q: %w", imgFile, err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close image file %q: %w", imgFile, closeErr)
	}

	l.logger.Info("downloaded image", "url", rawImgURL, "local", imgPath)
	return imgPath, nil
}

func (l loader) resolveUploadImgPath(rawImgURL string) (string, error) {
	imgURL, err := url.Parse(rawImgURL)
	if err != nil {
		return "", fmt.Errorf("parse image URL %q: %w", rawImgURL, err)
	}

	if !isUploadURL(imgURL, l.uploadURL) {
		return "", fmt.Errorf("image URL %q is outside upload URL %q", rawImgURL, l.uploadURL.String())
	}

	imgPath := strings.TrimPrefix(imgURL.Path, l.uploadURL.Path)
	if _, err = filepath.Localize(imgPath); err != nil {
		return "", fmt.Errorf("image URL %q has invalid upload path %q: %w", rawImgURL, imgPath, err)
	}

	return imgPath, nil
}

func isUploadURL(imageURL, uploadURL *url.URL) bool {
	if imageURL.Scheme != uploadURL.Scheme {
		return false
	}
	if imageURL.Host != uploadURL.Host {
		return false
	}
	if !strings.HasPrefix(imageURL.Path, uploadURL.Path) {
		return false
	}
	return true
}
