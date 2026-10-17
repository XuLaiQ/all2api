package media

import (
	"container/heap"
	"image"
	"math"
)

// inpaintTelea fills every pixel where mask is true using Telea's fast
// marching inpainting (A. Telea, "An Image Inpainting Technique Based on the
// Fast Marching Method", 2004), the algorithm behind cv2.INPAINT_TELEA.
// Pixels are filled from the mask boundary inwards in order of their distance
// T; each one is the average of already-known pixels within radius, weighted
// by direction (alignment with grad T), distance and level-set proximity.
//
// OpenCV additionally adds a normalized gradient term bounded to about one
// intensity level per pixel. It is omitted here: in testing it accumulated
// into visible drift across wide masks, while the weighted average alone
// reconstructs smooth backgrounds within a few levels. Alpha is untouched.
func inpaintTelea(img *image.RGBA, mask []bool, radius int) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if radius < 1 {
		radius = 1
	}
	const (
		known = iota
		band
		inside
	)
	const far = 1e6

	flags := make([]uint8, width*height)
	dist := make([]float64, width*height)
	values := make([][3]float64, width*height)
	for y := 0; y < height; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < width; x++ {
			index := y*width + x
			values[index] = [3]float64{float64(row[x*4]), float64(row[x*4+1]), float64(row[x*4+2])}
			if mask[index] {
				flags[index], dist[index] = inside, far
			}
		}
	}

	queue := &pixelHeap{}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			if flags[index] != known {
				continue
			}
			for _, n := range neighbors4 {
				nx, ny := x+n[0], y+n[1]
				if nx >= 0 && ny >= 0 && nx < width && ny < height && flags[ny*width+nx] == inside {
					flags[index] = band
					heap.Push(queue, pixelItem{index: index, dist: 0})
					break
				}
			}
		}
	}

	flagAt := func(x, y int) uint8 {
		if x < 0 || y < 0 || x >= width || y >= height {
			return inside
		}
		return flags[y*width+x]
	}
	distAt := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= width || y >= height {
			return far
		}
		return dist[y*width+x]
	}
	// solve is the first-order upwind solution of |grad T| = 1 from two
	// orthogonal neighbours (x1,y1) and (x2,y2).
	solve := func(x1, y1, x2, y2 int) float64 {
		f1, f2 := flagAt(x1, y1), flagAt(x2, y2)
		t1, t2 := distAt(x1, y1), distAt(x2, y2)
		switch {
		case f1 != inside && f2 != inside:
			delta := 2 - (t1-t2)*(t1-t2)
			if delta > 0 {
				r := math.Sqrt(delta)
				if s := (t1 + t2 - r) / 2; s >= t1 && s >= t2 {
					return s
				}
				if s := (t1 + t2 + r) / 2; s >= t1 && s >= t2 {
					return s
				}
			}
			return math.Min(t1, t2) + 1
		case f1 != inside:
			return t1 + 1
		case f2 != inside:
			return t2 + 1
		}
		return far
	}
	// gradient returns the central/one-sided difference of field at (x,y)
	// using only neighbours where field reports a value.
	gradient := func(x, y int, field func(int, int) (float64, bool)) (float64, float64) {
		axis := func(dx, dy int) float64 {
			center, _ := field(x, y)
			next, hasNext := field(x+dx, y+dy)
			prev, hasPrev := field(x-dx, y-dy)
			switch {
			case hasNext && hasPrev:
				return (next - prev) / 2
			case hasNext:
				return next - center
			case hasPrev:
				return center - prev
			}
			return 0
		}
		return axis(1, 0), axis(0, 1)
	}
	// filling is the pixel currently being inpainted. It stays flagged inside
	// until its colour is computed so neighbours never read its stale value,
	// but its freshly solved distance is needed for the gradient of T.
	filling := -1
	distField := func(x, y int) (float64, bool) {
		if x >= 0 && y >= 0 && x < width && y < height && y*width+x == filling {
			return dist[filling], true
		}
		if flagAt(x, y) == inside {
			return 0, false
		}
		return distAt(x, y), true
	}

	radiusSquared := radius * radius
	fill := func(x, y int) {
		gradTX, gradTY := gradient(x, y, distField)
		gradTNorm := math.Hypot(gradTX, gradTY)
		tq := dist[y*width+x]
		var sum [3]float64
		var weightSum float64
		for ky := y - radius; ky <= y+radius; ky++ {
			for kx := x - radius; kx <= x+radius; kx++ {
				dx, dy := x-kx, y-ky
				if dx*dx+dy*dy > radiusSquared || kx < 0 || ky < 0 || kx >= width || ky >= height {
					continue
				}
				kIndex := ky*width + kx
				if flags[kIndex] == inside || (kx == x && ky == y) {
					continue
				}
				length := math.Hypot(float64(dx), float64(dy))
				direction := 1.0
				if gradTNorm > 0 {
					direction = math.Abs(float64(dx)*gradTX+float64(dy)*gradTY) / (length * gradTNorm)
					if direction < 1e-6 {
						direction = 1e-6
					}
				}
				distance := 1 / (length * length)
				level := 1 / (1 + math.Abs(dist[kIndex]-tq))
				weight := direction * distance * level
				for channel := 0; channel < 3; channel++ {
					sum[channel] += weight * values[kIndex][channel]
				}
				weightSum += weight
			}
		}
		if weightSum == 0 {
			return
		}
		index := y*width + x
		for channel := 0; channel < 3; channel++ {
			values[index][channel] = clamp255(sum[channel] / weightSum)
		}
	}

	for queue.Len() > 0 {
		current := heap.Pop(queue).(pixelItem)
		cx, cy := current.index%width, current.index/width
		flags[current.index] = known
		for _, n := range neighbors4 {
			nx, ny := cx+n[0], cy+n[1]
			if nx < 0 || ny < 0 || nx >= width || ny >= height {
				continue
			}
			nIndex := ny*width + nx
			if flags[nIndex] != inside {
				continue
			}
			dist[nIndex] = math.Min(
				math.Min(solve(nx-1, ny, nx, ny-1), solve(nx+1, ny, nx, ny-1)),
				math.Min(solve(nx-1, ny, nx, ny+1), solve(nx+1, ny, nx, ny+1)),
			)
			filling = nIndex
			fill(nx, ny)
			filling = -1
			flags[nIndex] = band
			heap.Push(queue, pixelItem{index: nIndex, dist: dist[nIndex]})
		}
	}

	for y := 0; y < height; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < width; x++ {
			index := y*width + x
			if !mask[index] {
				continue
			}
			row[x*4] = uint8(math.Round(values[index][0]))
			row[x*4+1] = uint8(math.Round(values[index][1]))
			row[x*4+2] = uint8(math.Round(values[index][2]))
		}
	}
}

var neighbors4 = [4][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}}

func clamp255(value float64) float64 {
	return math.Max(0, math.Min(255, value))
}

type pixelItem struct {
	index int
	dist  float64
}

type pixelHeap []pixelItem

func (h pixelHeap) Len() int           { return len(h) }
func (h pixelHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h pixelHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pixelHeap) Push(value any)    { *h = append(*h, value.(pixelItem)) }
func (h *pixelHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}
