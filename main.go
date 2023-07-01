package main

import (
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/vorbis"
	"golang.org/x/image/draw"
	"golang.org/x/term"
)

var CHARS = []string{"  ", "░░", "▒▒", "▓▓", "██"}

type Renderer struct {
	mu           sync.Mutex
	frames       []string
	currentFrame int
	maxFrame     int
	lastFrame    [][]int
	width        int
	height       int
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

func RemoveIndex(s []string, index int) []string {
	return append(s[:index], s[index+1:]...)
}

func (r *Renderer) RenderFrame() bool {
	r.mu.Lock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	fmt.Print(r.frames[0])
	fmt.Printf("\033[%d;%dH", r.height, 0)
	fmt.Printf("Frame: %d | Memory: %dMiB | Buffer: %d", r.currentFrame, bToMb(m.Alloc), len(r.frames))
	r.currentFrame++

	// Remove the frame that has been drawn since it won't be
	// used anymore, its just taking unnecessary space.
	r.frames = RemoveIndex(r.frames, 0)

	defer r.mu.Unlock()
	return r.currentFrame == r.maxFrame
}

func (r *Renderer) Add(frameData string, i, bufsize int) {
	for {
		r.mu.Lock()
		if i-r.currentFrame < bufsize {
			break
		} else {
			r.mu.Unlock()
		}
	}
	r.frames = append(r.frames, frameData)
	r.mu.Unlock()
}

func FindImageFiles(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var fileNames []string
	for _, file := range entries {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".jpg") {
			continue
		}

		fileNames = append(fileNames, file.Name())
	}

	// Our image is named like 0001.png so string sorting sucks balls
	// Parse filename as int then sort it that way
	sort.SliceStable(fileNames, func(i, j int) bool {
		source, err := strconv.Atoi(strings.Split(fileNames[i], ".")[0])
		if err != nil {
			panic(err)
		}
		target, err := strconv.Atoi(strings.Split(fileNames[j], ".")[0])
		if err != nil {
			panic(err)
		}

		return source < target
	})

	return fileNames, nil
}

// Amazing float64 golang
func IntMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func GeneratePixels(w, h int, im image.Image, lastFrame [][]int) string {
	var sb strings.Builder
	// Resize image before processing anything
	bounds := im.Bounds()

	// We need to find the smallest dimension, whether its x or y
	newWidth := IntMin(w, bounds.Max.X)
	newHeight := IntMin(h, bounds.Max.Y)
	heightRatio := float64(newHeight) / float64(bounds.Max.Y)
	widthRatio := float64(newWidth) / float64(bounds.Max.X)

	if heightRatio < widthRatio {
		h = newHeight
		w = int(float64(bounds.Max.X) * heightRatio)
	} else {
		w = newWidth
		h = int(float64(bounds.Max.Y) * widthRatio)
	}
	// Rescale the image to new width and height
	newImg := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.NearestNeighbor.Scale(newImg, newImg.Rect, im, bounds, draw.Over, nil)

	// We don't need the image anymore, just throw it to GC
	im = nil

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := newImg.At(x, y).RGBA()

			// https://en.wikipedia.org/wiki/Grayscale
			// why the hell is it ranging from 0 - 65535 instead of 0-255 wtf
			lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 65535
			idx := int(math.Max(math.Min(4, lum*5), 0))
			if lastFrame[y][x] != idx {
				// fmt.Printf("%d, %d", y, x)
				lastFrame[y][x] = idx
				sb.WriteString(fmt.Sprintf("\033[%d;%dH%s", y, x*2, CHARS[idx]))
			}
		}
	}
	output := sb.String()
	return output
}
func GenerateFrames(baseDir string, w, h, bufsize int, fileNames []string, r *Renderer) {
	for i, fname := range fileNames {
		reader, err := os.Open(baseDir + "/" + fname)
		if err != nil {
			panic(err)
		}

		im, _, err := image.Decode(reader)
		if err != nil {
			panic(err)
		}
		reader.Close()

		// Dividing width by 2 since we use 2 chars to draw each pixel
		pixels := GeneratePixels(w/2, h-1, im, r.lastFrame)
		r.Add(pixels, i, bufsize)
	}
}

func main() {
	delay := flag.Int("delay", 0, "Delay to be added before render starts")
	bufsize := flag.Int("bufsize", 500, "How many frames to buffer")
	flag.Parse()

	framesDir := flag.Arg(0)
	audioPath := flag.Arg(1)

	if framesDir == "" || audioPath == "" {
		flag.PrintDefaults()
		return
	}

	// Terminal size
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		panic(err)
	}

	fileNames, err := FindImageFiles(framesDir)
	if err != nil {
		panic(err)
	}

	baseArray := make([][]int, h)
	for y := 0; y < h; y++ {
		xArr := make([]int, w)
		for x := 0; x < w; x++ {
			xArr[x] = -1
		}
		baseArray[y] = xArr
	}
	// Create our renderer
	r := Renderer{
		frames:       make([]string, 0),
		currentFrame: 0,
		maxFrame:     len(fileNames),
		lastFrame:    baseArray,
		width:        w,
		height:       h,
	}

	// Let the frames be generated asynchronously
	go GenerateFrames(framesDir, w, h, *bufsize, fileNames, &r)
	ticker := time.NewTicker(1000000 / 30 * time.Microsecond)

	// Play audio stuff below
	f, err := os.Open(audioPath)
	if err != nil {
		panic(err)
	}
	streamer, format, err := vorbis.Decode(f)
	if err != nil {
		panic(err)
	}
	defer streamer.Close()

	speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
	speaker.Play(streamer)

	// Sleep during delay
	time.Sleep(time.Duration(*delay) * time.Millisecond)
	for range ticker.C {
		// If RenderFrame() returns true, then we are at the end
		if r.RenderFrame() {
			break
		}
	}
}
