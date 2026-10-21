package main

import (
	"strconv"
	"strings"
)

// extractors maps each collection we subscribe to onto the function that
// pulls matchable text out of its records. Adding a collection here is all it
// takes to start scanning it.
var extractors = map[string]func(map[string]any) string{
	"app.bsky.feed.post":     postText,
	"app.bsky.actor.profile": profileText,
}

// profileText pulls the matchable text out of an app.bsky.actor.profile
// record: display name, bio, pronouns, and website.
func profileText(rec map[string]any) string {
	var parts []string
	for _, k := range []string{"displayName", "description", "pronouns", "website"} {
		if s, ok := rec[k].(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// postText pulls the matchable text out of a decoded app.bsky.feed.post
// record: the body, image alt text, link-card title/description/URL, and
// facet link URIs. Everything is joined with newlines so phrases can't
// accidentally span two fields.
func postText(rec map[string]any) string {
	var parts []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}

	add(rec["text"])

	if facets, ok := rec["facets"].([]any); ok {
		for _, f := range facets {
			fm, _ := f.(map[string]any)
			feats, _ := fm["features"].([]any)
			for _, ft := range feats {
				ftm, _ := ft.(map[string]any)
				if ftm["$type"] == "app.bsky.richtext.facet#link" {
					add(ftm["uri"])
				}
			}
		}
	}

	if embed, ok := rec["embed"].(map[string]any); ok {
		embedText(embed, add)
		// recordWithMedia nests the media embed one level down.
		if media, ok := embed["media"].(map[string]any); ok {
			embedText(media, add)
		}
	}

	return strings.Join(parts, "\n")
}

func embedText(embed map[string]any, add func(any)) {
	if imgs, ok := embed["images"].([]any); ok {
		for _, img := range imgs {
			if im, ok := img.(map[string]any); ok {
				add(im["alt"])
			}
		}
	}
	if ext, ok := embed["external"].(map[string]any); ok {
		add(ext["uri"])
		add(ext["title"])
		add(ext["description"])
	}
	add(embed["alt"]) // video embeds
}

// mediaSummary describes the media attached to a record ("2 images, 1 video")
// without exposing any of it. Media is never forwarded downstream; this only
// tells a human that something is there.
func mediaSummary(rec map[string]any) string {
	var images, videos, other int
	if _, ok := rec["avatar"].(map[string]any); ok {
		other++
	}
	if _, ok := rec["banner"].(map[string]any); ok {
		other++
	}
	count := func(embed map[string]any) {
		if imgs, ok := embed["images"].([]any); ok {
			images += len(imgs)
		}
		if _, ok := embed["video"].(map[string]any); ok {
			videos++
		}
		if ext, ok := embed["external"].(map[string]any); ok {
			if _, ok := ext["thumb"].(map[string]any); ok {
				other++
			}
		}
	}
	if embed, ok := rec["embed"].(map[string]any); ok {
		count(embed)
		if media, ok := embed["media"].(map[string]any); ok {
			count(media)
		}
	}

	var parts []string
	plural := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, strconv.Itoa(n)+" "+many)
		}
	}
	plural(images, "image", "images")
	plural(videos, "video", "videos")
	plural(other, "other image (avatar/banner/thumbnail)", "other images (avatar/banner/thumbnail)")
	return strings.Join(parts, ", ")
}
