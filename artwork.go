package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	data, err := httpGet(ctx, u)
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
func showArtwork(ctx context.Context, title string, info ShowInfo) *artwork {
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

// coverName is the file Plex and other players look for as album art.
func (a *artwork) coverName() string {
	if a.mime == "image/png" {
		return "cover.png"
	}
	return "cover.jpg"
}

// writeCover saves the artwork into dir, unless a cover is already there.
func (a *artwork) writeCover(dir string) error {
	path := filepath.Join(dir, a.coverName())
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp := filepath.Join(dir, "."+a.coverName()+".tmp")
	if err := os.WriteFile(tmp, a.data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// picture adds an APIC frame with the artwork as the front cover.
func (t *id3Tag) picture(a *artwork) {
	if a == nil {
		return
	}
	body := []byte{0x00} // ISO-8859-1 description: the most widely supported
	body = append(body, a.mime...)
	body = append(body, 0x00, 0x03, 0x00) // MIME terminator, "front cover", empty description
	body = append(body, a.data...)
	t.frames.Write(id3Frame("APIC", body))
}
