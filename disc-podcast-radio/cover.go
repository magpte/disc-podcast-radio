package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resizeImage downscales an image to fit within maxDim x maxDim while preserving aspect ratio.
func resizeImage(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return src
	}
	var nw, nh int
	if w > h {
		nw = maxDim
		nh = (h * maxDim) / w
	} else {
		nh = maxDim
		nw = (w * maxDim) / h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := b.Min.Y + (y*h)/nh
		for x := 0; x < nw; x++ {
			sx := b.Min.X + (x*w)/nw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// defaultCoverJPEG generates a stylish 320x320 vinyl disc cover art for podcast/radio.
func defaultCoverJPEG(mediaType, title, subtitle string) []byte {
	const size = 320
	const cx, cy = 160, 160
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Dark sleek background
	bgColor := color.RGBA{18, 19, 22, 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	// Vinyl disc body
	discColor := color.RGBA{24, 26, 30, 255}
	grooveColor := color.RGBA{38, 41, 48, 255}
	grooveLight := color.RGBA{48, 52, 60, 255}

	// Draw disc
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			dist := math.Hypot(dx, dy)
			if dist <= 154 {
				// Outer rim
				if dist > 151 {
					img.Set(x, y, grooveLight)
				} else if dist > 55 {
					// Concentric groove bands
					grooveBand := int(dist) % 14
					if grooveBand == 0 || grooveBand == 1 {
						img.Set(x, y, grooveColor)
					} else {
						img.Set(x, y, discColor)
					}
				}
			}
		}
	}

	// Center record label
	var labelColor, labelAccent color.RGBA
	if mediaType == "podcast" {
		labelColor = color.RGBA{14, 165, 233, 255}   // Cyan/Sky blue
		labelAccent = color.RGBA{2, 132, 199, 255}
	} else {
		labelColor = color.RGBA{245, 158, 11, 255}  // Amber
		labelAccent = color.RGBA{217, 119, 6, 255}
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			dist := math.Hypot(dx, dy)
			if dist <= 55 {
				if dist > 51 {
					img.Set(x, y, labelAccent)
				} else if dist > 18 {
					img.Set(x, y, labelColor)
				} else if dist > 14 {
					img.Set(x, y, color.RGBA{240, 240, 240, 255})
				} else {
					// Spindle hole
					img.Set(x, y, bgColor)
				}
			}
		}
	}

	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

// prepareCoverImage downloads and normalizes the cover image, or falls back to generated vinyl art.
func (a *App) prepareCoverImage(ctx context.Context, coverURL, mediaType, title, subtitle string) []byte {
	coverURL = strings.TrimSpace(coverURL)
	if coverURL != "" {
		cacheKey := stableHash(coverURL, 16) + ".jpg"
		cacheDir := "/tmp/disc-podcast-radio/covers"
		_ = os.MkdirAll(cacheDir, 0755)
		cachePath := filepath.Join(cacheDir, cacheKey)

		// Check if already cached on disk
		if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
			a.log.Infof("[Cover] 使用本地缓存封面: %s (%d bytes)", cacheKey, len(data))
			return data
		}

		// Asynchronous fetch and decode to prevent blocking playback initiation
		a.log.Infof("[Cover] 封面未命中缓存，启动后台异步抓取与缩放: %s", coverURL)
		go a.asyncFetchCover(coverURL, cachePath)
	}

	return defaultCoverJPEG(mediaType, title, subtitle)
}

func (a *App) asyncFetchCover(coverURL, cachePath string) {
	fetchCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	data, _, err := a.fetchBytes(fetchCtx, coverURL, 8<<20)
	if err == nil && len(data) > 0 {
		img, _, decErr := image.Decode(bytes.NewReader(data))
		if decErr == nil && img != nil {
			scaled := resizeImage(img, 360)
			var buf bytes.Buffer
			if encErr := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 88}); encErr == nil {
				res := buf.Bytes()
				_ = os.WriteFile(cachePath, res, 0644)
				_ = os.MkdirAll("/usr/data/fiio", 0755)
				_ = os.WriteFile("/usr/data/fiio/net_cover.jpg", res, 0644)
				a.currentCoverMu.Lock()
				a.currentCoverBytes = res
				a.currentCoverMu.Unlock()
				a.log.Infof("[Cover] 异步下载并缓存封面成功 (%s -> %d bytes)", coverURL, len(res))
				return
			}
		} else {
			a.log.Warnf("[Cover] 异步封面图像解码失败 (%s): %v", coverURL, decErr)
		}
	} else {
		a.log.Warnf("[Cover] 异步下载封面失败 (%s): %v", coverURL, err)
	}
}
