package recorder

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"wcbn-recorder/internal/httpx"
	"wcbn-recorder/internal/spinitron"
)

// artwork is a show's image, embedded in every file and saved as the show
// folder's cover.
type artwork struct {
	mime string // image/jpeg or image/png
	data []byte
}

// maxArtwork caps the download; Spinitron and Flickr images are far smaller.
const maxArtwork = 10 << 20

// fetchArtwork downloads an image, accepting only JPEG and PNG (what ID3
// players and Plex support), judged by content rather than headers.
func fetchArtwork(ctx context.Context, u string) (*artwork, error) {
	data, err := httpx.Get(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtwork {
		return nil, fmt.Errorf("%s: image too large", u)
	}
	switch mime := http.DetectContentType(data); mime {
	case "image/jpeg", "image/png":
		return &artwork{mime: mime, data: data}, nil
	default:
		return nil, fmt.Errorf("%s: %s isn't JPEG or PNG", u, mime)
	}
}

// showArtwork fetches a show's image, falling back to the station logo when
// it's missing or gone (some shows link Flickr images that no longer exist).
func showArtwork(ctx context.Context, title string, info spinitron.ShowInfo) *artwork {
	for _, u := range []string{info.Image, info.StationImage} {
		if u == "" {
			continue
		}
		a, err := fetchArtwork(ctx, u)
		if err == nil {
			if u != info.Image {
				log.Printf("using the station logo as artwork for %q", title)
			}
			return a
		}
		log.Printf("couldn't load artwork for %q: %v", title, err)
	}
	return nil
}
