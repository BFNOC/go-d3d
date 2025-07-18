package outputduplication

import (
	"errors"
	"fmt"
	"image"
	"image/color"

	"unsafe"

	"github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/dxgi"
	"github.com/kirides/go-d3d/outputduplication/swizzle"
)

type PointerInfo struct {
	pos dxgi.POINT

	size           dxgi.POINT
	shapeInBuffer  []byte
	shapeOutBuffer *image.RGBA
	visible        bool
}

type OutputDuplicator struct {
	device            *d3d11.ID3D11Device
	deviceCtx         *d3d11.ID3D11DeviceContext
	outputDuplication *dxgi.IDXGIOutputDuplication
	dxgiOutput        *dxgi.IDXGIOutput5

	stagedTex  *d3d11.ID3D11Texture2D
	surface    *dxgi.IDXGISurface
	mappedRect dxgi.DXGI_MAPPED_RECT
	size       dxgi.POINT

	pointerInfo PointerInfo
	// Always draw pointer onto the final image when calling GetImage
	DrawPointer bool
	// Update pointer information when it changes, used with DrawCursor(image)
	UpdatePointerInfo bool

	// TODO: handle DPI? Do we need it?
	dirtyRects    []dxgi.RECT
	movedRects    []dxgi.DXGI_OUTDUPL_MOVE_RECT
	acquiredFrame bool
	needsSwizzle  bool // in case we use DuplicateOutput1, swizzle is not neccessery
}

func (dup *OutputDuplicator) initializeStage(texture *d3d11.ID3D11Texture2D) int32 {

	/*
		TODO: Only do this on changes!
	*/
	var hr int32
	desc := d3d11.D3D11_TEXTURE2D_DESC{}
	hr = texture.GetDesc(&desc)
	if d3d.HRESULT(hr).Failed() {
		return hr
	}

	desc.Usage = d3d11.D3D11_USAGE_STAGING
	desc.CPUAccessFlags = d3d11.D3D11_CPU_ACCESS_READ
	desc.BindFlags = 0
	desc.MipLevels = 1
	desc.ArraySize = 1
	desc.MiscFlags = 0
	desc.SampleDesc.Count = 1

	hr = dup.device.CreateTexture2D(&desc, &dup.stagedTex)
	if d3d.HRESULT(hr).Failed() {
		return hr
	}

	hr = dup.stagedTex.QueryInterface(dxgi.IID_IDXGISurface, &dup.surface)
	if d3d.HRESULT(hr).Failed() {
		return hr
	}
	dup.size = dxgi.POINT{X: int32(desc.Width), Y: int32(desc.Height)}

	return 0
}

func (dup *OutputDuplicator) Release() {
	dup.ReleaseFrame()
	if dup.stagedTex != nil {
		dup.stagedTex.Release()
		dup.stagedTex = nil
	}
	if dup.surface != nil {
		dup.surface.Release()
		dup.surface = nil
	}
	if dup.outputDuplication != nil {
		dup.outputDuplication.Release()
		dup.outputDuplication = nil
	}
	if dup.dxgiOutput != nil {
		dup.dxgiOutput.Release()
		dup.dxgiOutput = nil
	}
}

var ErrNoImageYet = errors.New("no image yet")

type unmapFn func() int32

func (dup *OutputDuplicator) ReleaseFrame() {
	if dup.acquiredFrame {
		dup.outputDuplication.ReleaseFrame()
		dup.acquiredFrame = false
	}
}

// returns DXGI_FORMAT_B8G8R8A8_UNORM data
func (dup *OutputDuplicator) Snapshot(timeoutMs uint) (unmapFn, *dxgi.DXGI_MAPPED_RECT, *dxgi.POINT, error) {
	var hr int32
	desc := dxgi.DXGI_OUTDUPL_DESC{}
	hr = dup.outputDuplication.GetDesc(&desc)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, nil, nil, fmt.Errorf("failed to get the description. %w", hr)
	}

	if desc.DesktopImageInSystemMemory != 0 {
		// TODO: Figure out WHEN exactly this can occur, and if we can make use of it
		dup.size = dxgi.POINT{X: int32(desc.ModeDesc.Width), Y: int32(desc.ModeDesc.Height)}
		hr = dup.outputDuplication.MapDesktopSurface(&dup.mappedRect)
		if hr := d3d.HRESULT(hr); !hr.Failed() {
			return dup.outputDuplication.UnMapDesktopSurface, &dup.mappedRect, &dup.size, nil
		}
	}

	var desktop *dxgi.IDXGIResource
	var frameInfo dxgi.DXGI_OUTDUPL_FRAME_INFO

	dup.ReleaseFrame()
	hrF := dup.outputDuplication.AcquireNextFrame(uint32(timeoutMs), &frameInfo, &desktop)
	dup.acquiredFrame = true
	if hr := d3d.HRESULT(hrF); hr.Failed() {
		if hr == d3d.DXGI_ERROR_WAIT_TIMEOUT {
			return nil, nil, nil, ErrNoImageYet
		}
		return nil, nil, nil, fmt.Errorf("failed to AcquireNextFrame. %w", d3d.HRESULT(hrF))
	}

	defer dup.ReleaseFrame()
	defer desktop.Release()

	if dup.UpdatePointerInfo {
		if err := dup.updatePointer(&frameInfo); err != nil {
			return nil, nil, nil, err
		}
	}

	if frameInfo.AccumulatedFrames == 0 {
		return nil, nil, nil, ErrNoImageYet
	}
	var desktop2d *d3d11.ID3D11Texture2D
	hr = desktop.QueryInterface(d3d11.IID_ID3D11Texture2D, &desktop2d)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, nil, nil, fmt.Errorf("failed to QueryInterface(iid_ID3D11Texture2D, ...). %w", hr)
	}
	defer desktop2d.Release()

	if dup.stagedTex == nil {
		hr = dup.initializeStage(desktop2d)
		if hr := d3d.HRESULT(hr); hr.Failed() {
			return nil, nil, nil, fmt.Errorf("failed to InitializeStage. %w", hr)
		}
	}

	// NOTE: we could use a single, large []byte buffer and use it as storage for moved rects & dirty rects
	if frameInfo.TotalMetadataBufferSize > 0 {
		// Handling moved / dirty rects, to reduce GPU<->CPU memory copying
		moveRectsRequired := uint32(1)
		for {
			if len(dup.movedRects) < int(moveRectsRequired) {
				dup.movedRects = make([]dxgi.DXGI_OUTDUPL_MOVE_RECT, moveRectsRequired)
			}
			hr = dup.outputDuplication.GetFrameMoveRects(dup.movedRects, &moveRectsRequired)
			if hr := d3d.HRESULT(hr); hr.Failed() {
				if hr == d3d.DXGI_ERROR_MORE_DATA {
					continue
				}
				return nil, nil, nil, fmt.Errorf("failed to GetFrameMoveRects. %w", d3d.HRESULT(hr))
			}
			dup.movedRects = dup.movedRects[:moveRectsRequired]
			break
		}

		dirtyRectsRequired := uint32(1)
		for {
			if len(dup.dirtyRects) < int(dirtyRectsRequired) {
				dup.dirtyRects = make([]dxgi.RECT, dirtyRectsRequired)
			}
			hr = dup.outputDuplication.GetFrameDirtyRects(dup.dirtyRects, &dirtyRectsRequired)
			if hr := d3d.HRESULT(hr); hr.Failed() {
				if hr == d3d.DXGI_ERROR_MORE_DATA {
					continue
				}
				return nil, nil, nil, fmt.Errorf("failed to GetFrameDirtyRects. %w", d3d.HRESULT(hr))
			}
			dup.dirtyRects = dup.dirtyRects[:dirtyRectsRequired]
			break
		}

		box := d3d11.D3D11_BOX{
			Front: 0,
			Back:  1,
		}
		if len(dup.movedRects) == 0 {
			for i := 0; i < len(dup.dirtyRects); i++ {
				box.Left = uint32(dup.dirtyRects[i].Left)
				box.Top = uint32(dup.dirtyRects[i].Top)
				box.Right = uint32(dup.dirtyRects[i].Right)
				box.Bottom = uint32(dup.dirtyRects[i].Bottom)

				dup.deviceCtx.CopySubresourceRegion2D(dup.stagedTex, 0, box.Left, box.Top, 0, desktop2d, 0, &box)
			}
		} else {
			// TODO: handle moved rects, then dirty rects
			// for now, just update the whole image instead
			dup.deviceCtx.CopyResource2D(dup.stagedTex, desktop2d)
		}
	} else {
		// no frame metadata, copy whole image
		dup.deviceCtx.CopyResource2D(dup.stagedTex, desktop2d)
		if !dup.needsSwizzle {
			dup.needsSwizzle = true
		}
		print("no frame metadata\n")
	}

	hr = dup.surface.Map(&dup.mappedRect, dxgi.DXGI_MAP_READ)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, nil, nil, fmt.Errorf("failed to surface_.Map(...). %v", hr)
	}
	return dup.surface.Unmap, &dup.mappedRect, &dup.size, nil
}

func (dup *OutputDuplicator) DrawCursor(img *image.RGBA) error {
	return dup.drawPointer(img)
}

func (dup *OutputDuplicator) GetImage(img *image.RGBA, timeoutMs uint) error {
	unmap, mappedRect, size, err := dup.Snapshot(timeoutMs)
	if err != nil {
		return err
	}
	defer unmap()

	// docs are unclear, but pitch is the total width of each row
	dataSize := int(mappedRect.Pitch) * int(size.Y)
	data := unsafe.Slice((*byte)(mappedRect.PBits), dataSize)

	contentWidth := int(size.X) * 4
	dataWidth := int(mappedRect.Pitch)

	var imgStart, dataStart, dataEnd int
	// copy source bytes into image.RGBA.Pix, skipping padding
	for i := 0; i < int(size.Y); i++ {
		dataEnd = dataStart + contentWidth
		copy(img.Pix[imgStart:], data[dataStart:dataEnd])
		imgStart += contentWidth
		dataStart += dataWidth
	}

	if dup.needsSwizzle {
		swizzle.BGRA(img.Pix)
	}

	if dup.DrawPointer {
		dup.drawPointer(img)
	}

	return nil
}

func (dup *OutputDuplicator) updatePointer(info *dxgi.DXGI_OUTDUPL_FRAME_INFO) error {
	if info.LastMouseUpdateTime == 0 {
		return nil
	}
	dup.pointerInfo.visible = info.PointerPosition.Visible != 0
	dup.pointerInfo.pos = info.PointerPosition.Position

	if info.PointerShapeBufferSize != 0 {
		// new shape
		if len(dup.pointerInfo.shapeInBuffer) < int(info.PointerShapeBufferSize) {
			dup.pointerInfo.shapeInBuffer = make([]byte, info.PointerShapeBufferSize)
		}
		var requiredSize uint32
		var pointerInfo dxgi.DXGI_OUTDUPL_POINTER_SHAPE_INFO

		hr := dup.outputDuplication.GetFramePointerShape(info.PointerShapeBufferSize,
			dup.pointerInfo.shapeInBuffer,
			&requiredSize,
			&pointerInfo,
		)
		if hr != 0 {
			return fmt.Errorf("unable to obtain frame pointer shape")
		}
		neededSize := int(pointerInfo.Width) * int(pointerInfo.Height/2) * 4
		dup.pointerInfo.shapeOutBuffer = image.NewRGBA(image.Rect(0, 0, int(pointerInfo.Width), int(pointerInfo.Height)))
		if len(dup.pointerInfo.shapeOutBuffer.Pix) < int(neededSize) {
			dup.pointerInfo.shapeOutBuffer.Pix = make([]byte, neededSize)
		}

		switch pointerInfo.Type {
		case dxgi.DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME:
			width := int(pointerInfo.Width)
			height := int(pointerInfo.Height) / 2 // Corrected height!

			dup.pointerInfo.size = dxgi.POINT{X: int32(width), Y: int32(height)}

			xor_offset := pointerInfo.Pitch * uint32(height)
			andMap := dup.pointerInfo.shapeInBuffer
			xorMap := dup.pointerInfo.shapeInBuffer[xor_offset:]
			out_pixels := dup.pointerInfo.shapeOutBuffer.Pix
			widthBytes := (width + 7) / 8

			for j := 0; j < height; j++ {
				for i := 0; i < width; i++ {
					byteIndex := j*widthBytes + i/8
					bitMask := byte(0x80 >> (i % 8))

					andBit := (andMap[byteIndex] & bitMask) != 0
					xorBit := (xorMap[byteIndex] & bitMask) != 0

					outIndex := (j*width + i) * 4
					// var r, g, b, a byte

					switch {
					case !andBit && !xorBit: // Transparent
						// 	r, g, b, a = 0, 0, 0, 0
						*(*uint32)(unsafe.Pointer(&out_pixels[outIndex])) = 0x00000000
					case !andBit && xorBit: // Inverted (white)
						// r, g, b, a = 255, 255, 255, 255
						*(*uint32)(unsafe.Pointer(&out_pixels[outIndex])) = 0xFFFFFFFF
					case andBit && !xorBit: // Black
						// 	r, g, b, a = 0, 0, 0, 255 // causes a black plane to be rendered alongside the cursors image
						// out_pixels[outIndex+0] = 0   // r
						// out_pixels[outIndex+1] = 0   // g
						// out_pixels[outIndex+2] = 0   // b
						// out_pixels[outIndex+3] = 255 // a
						*(*uint32)(unsafe.Pointer(&out_pixels[outIndex])) = 0x00000000
					case andBit && xorBit: // Inverted (adaptive color)
						// Start with black, will be made adaptive in drawPointer based on background
						*(*uint32)(unsafe.Pointer(&out_pixels[outIndex])) = 0xFF000000
					}
				}
			}

		case dxgi.DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR:
			dup.pointerInfo.size = dxgi.POINT{X: int32(pointerInfo.Width), Y: int32(pointerInfo.Height)}

			out, in := dup.pointerInfo.shapeOutBuffer.Pix, dup.pointerInfo.shapeInBuffer
			width := int(pointerInfo.Width)
			for j := 0; j < int(pointerInfo.Height); j++ {
				// Output buffer stride: width * 4 bytes per pixel (RGBA)
				tout := out[j*width*4 : (j+1)*width*4]
				// Input buffer stride: uses pointerInfo.Pitch
				tin := in[j*int(pointerInfo.Pitch) : j*int(pointerInfo.Pitch)+width*4]
				copy(tout, tin)
			}

			// Convert BGRA to RGBA
			for i := 0; i < len(out); i += 4 {
				// Swap B and R channels: out[i] is B, out[i+2] is R
				out[i], out[i+2] = out[i+2], out[i]
			}
		case dxgi.DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR:
			dup.pointerInfo.size = dxgi.POINT{X: int32(pointerInfo.Width), Y: int32(pointerInfo.Height)}

			// TODO: Properly add mask
			out, in := dup.pointerInfo.shapeOutBuffer.Pix, dup.pointerInfo.shapeInBuffer
			width := int(pointerInfo.Width)
			for j := 0; j < int(pointerInfo.Height); j++ {
				// Output buffer stride: width * 4 bytes per pixel (RGBA)
				tout := out[j*width*4 : (j+1)*width*4]
				// Input buffer stride: uses pointerInfo.Pitch
				tin := in[j*int(pointerInfo.Pitch) : j*int(pointerInfo.Pitch)+width*4]
				copy(tout, tin)
			}

			// Convert BGRA to RGBA
			for i := 0; i < len(out); i += 4 {
				// Swap B and R channels: out[i] is B, out[i+2] is R
				out[i], out[i+2] = out[i+2], out[i]
			}
		default:
			dup.pointerInfo.size = dxgi.POINT{X: 0, Y: 0}
			return fmt.Errorf("unsupported type %v", pointerInfo.Type)
		}
	}
	return nil
}

// analyzeBackgroundBrightness checks the area around the cursor to determine if background is light or dark
func (dup *OutputDuplicator) analyzeBackgroundBrightness(img *image.RGBA) bool {
	// Sample area around cursor position
	sampleSize := 20
	startX := int(dup.pointerInfo.pos.X) - sampleSize
	startY := int(dup.pointerInfo.pos.Y) - sampleSize
	endX := int(dup.pointerInfo.pos.X) + int(dup.pointerInfo.size.X) + sampleSize
	endY := int(dup.pointerInfo.pos.Y) + int(dup.pointerInfo.size.Y) + sampleSize

	// Ensure bounds are within image
	if startX < 0 {
		startX = 0
	}
	if startY < 0 {
		startY = 0
	}
	if endX >= img.Bounds().Max.X {
		endX = img.Bounds().Max.X - 1
	}
	if endY >= img.Bounds().Max.Y {
		endY = img.Bounds().Max.Y - 1
	}

	var totalBrightness uint64
	var pixelCount uint64

	for y := startY; y <= endY; y++ {
		for x := startX; x <= endX; x++ {
			// Skip cursor area itself
			if x >= int(dup.pointerInfo.pos.X) && x < int(dup.pointerInfo.pos.X)+int(dup.pointerInfo.size.X) &&
				y >= int(dup.pointerInfo.pos.Y) && y < int(dup.pointerInfo.pos.Y)+int(dup.pointerInfo.size.Y) {
				continue
			}

			r, g, b, _ := img.At(x, y).RGBA()
			// Calculate BT.601 luminance using standard formula
			brightness := uint64(0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8))
			totalBrightness += brightness
			pixelCount++
		}
	}

	if pixelCount == 0 {
		return false // Default to dark background
	}

	avgBrightness := totalBrightness / pixelCount
	// Lower threshold to better detect dark themes like VSCode
	// If average brightness > 80 (roughly 1/3 of 0-255 range), consider it light background
	// This helps distinguish between dark themes (30-60) and light themes (200-255)
	// For example:
	// cmd.exe avgBrightness=15
	// VSCode Default Dark Theme avgBrightness=31

	return avgBrightness > 80
}

func (dup *OutputDuplicator) drawPointer(img *image.RGBA) error {
	isLightBackground := dup.analyzeBackgroundBrightness(img)

	for j := 0; j < int(dup.pointerInfo.size.Y); j++ {
		for i := 0; i < int(dup.pointerInfo.size.X); i++ {
			col := dup.pointerInfo.shapeOutBuffer.At(i, j)
			r, g, b, a := col.RGBA()
			if a == 0 {
				// just dont draw invisible pixel?
				// TODO: correctly apply mask
				continue
			}

			// For inverted cursor pixels (text cursor), adapt color based on background
			// Check for black pixels that came from monochrome cursor "andBit && xorBit" case
			if r == 0 && g == 0 && b == 0 && a == 0xFFFF {
				if isLightBackground {
					// Keep black cursor on light background
					col = color.RGBA{R: 0, G: 0, B: 0, A: 255}
				} else {
					// Use white cursor on dark background
					col = color.RGBA{R: 255, G: 255, B: 255, A: 255}
				}
			}

			img.Set(int(dup.pointerInfo.pos.X)+i, int(dup.pointerInfo.pos.Y)+j, col)
		}
	}
	return nil
}

func (ddup *OutputDuplicator) GetBounds() (image.Rectangle, error) {
	desc := dxgi.DXGI_OUTPUT_DESC{}
	hr := ddup.dxgiOutput.GetDesc(&desc)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return image.Rectangle{}, fmt.Errorf("failed at dxgiOutput.GetDesc. %w", hr)
	}

	return image.Rect(int(desc.DesktopCoordinates.Left), int(desc.DesktopCoordinates.Top), int(desc.DesktopCoordinates.Right), int(desc.DesktopCoordinates.Bottom)), nil
}

func newIDXGIOutputDuplicationFormat(device *d3d11.ID3D11Device, deviceCtx *d3d11.ID3D11DeviceContext, output uint, format dxgi.DXGI_FORMAT) (*OutputDuplicator, error) {
	// DEBUG

	var d3dDebug *d3d11.ID3D11Debug
	hr := device.QueryInterface(d3d11.IID_ID3D11Debug, &d3dDebug)
	if hr := d3d.HRESULT(hr); !hr.Failed() {
		defer d3dDebug.Release()

		var d3dInfoQueue *d3d11.ID3D11InfoQueue
		hr := d3dDebug.QueryInterface(d3d11.IID_ID3D11InfoQueue, &d3dInfoQueue)
		if hr := d3d.HRESULT(hr); hr.Failed() {
			return nil, fmt.Errorf("failed at device.QueryInterface. %w", hr)
		}
		defer d3dInfoQueue.Release()
		// defer d3dDebug.ReportLiveDeviceObjects(D3D11_RLDO_SUMMARY | D3D11_RLDO_DETAIL)
	}

	var dxgiDevice1 *dxgi.IDXGIDevice1
	hr = device.QueryInterface(dxgi.IID_IDXGIDevice1, &dxgiDevice1)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, fmt.Errorf("failed at device.QueryInterface. %w", hr)
	}
	defer dxgiDevice1.Release()

	var pdxgiAdapter unsafe.Pointer
	hr = dxgiDevice1.GetParent(dxgi.IID_IDXGIAdapter1, &pdxgiAdapter)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, fmt.Errorf("failed at dxgiDevice1.GetAdapter. %w", hr)
	}
	dxgiAdapter := (*dxgi.IDXGIAdapter1)(pdxgiAdapter)
	defer dxgiAdapter.Release()

	var dxgiOutput *dxgi.IDXGIOutput
	// const DXGI_ERROR_NOT_FOUND = 0x887A0002
	hr = int32(dxgiAdapter.EnumOutputs(uint32(output), &dxgiOutput))
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, fmt.Errorf("failed at dxgiAdapter.EnumOutputs. %w", hr)
	}
	defer dxgiOutput.Release()

	var dxgiOutput5 *dxgi.IDXGIOutput5
	hr = dxgiOutput.QueryInterface(dxgi.IID_IDXGIOutput5, &dxgiOutput5)
	if hr := d3d.HRESULT(hr); hr.Failed() {
		return nil, fmt.Errorf("failed at dxgiOutput.QueryInterface. %w", hr)
	}

	var dup *dxgi.IDXGIOutputDuplication
	hr = dxgiOutput5.DuplicateOutput1(dxgiDevice1, 0, []dxgi.DXGI_FORMAT{
		format,
	}, &dup)
	needsSwizzle := false
	if hr := d3d.HRESULT(hr); hr.Failed() {
		needsSwizzle = true
		// fancy stuff not supported :/
		// fmt.Printf("Info: failed to use dxgiOutput5.DuplicateOutput1, falling back to dxgiOutput1.DuplicateOutput. Missing manifest with DPI awareness set to \"PerMonitorV2\"? %v\n", _DXGI_ERROR(hr))
		var dxgiOutput1 *dxgi.IDXGIOutput1
		hr := dxgiOutput.QueryInterface(dxgi.IID_IDXGIOutput1, &dxgiOutput1)
		if hr := d3d.HRESULT(hr); hr.Failed() {
			dxgiOutput5.Release()
			return nil, fmt.Errorf("failed at dxgiOutput.QueryInterface. %w", hr)
		}
		defer dxgiOutput1.Release()
		hr = dxgiOutput1.DuplicateOutput(dxgiDevice1, &dup)
		if hr := d3d.HRESULT(hr); hr.Failed() {
			dxgiOutput5.Release()
			return nil, fmt.Errorf("failed at dxgiOutput1.DuplicateOutput. %w", hr)
		}
	}

	return &OutputDuplicator{device: device, deviceCtx: deviceCtx, outputDuplication: dup, needsSwizzle: needsSwizzle, dxgiOutput: dxgiOutput5}, nil
}

// NewIDXGIOutputDuplication creates a new OutputDuplicator
func NewIDXGIOutputDuplication(device *d3d11.ID3D11Device, deviceCtx *d3d11.ID3D11DeviceContext, output uint) (*OutputDuplicator, error) {
	return newIDXGIOutputDuplicationFormat(device, deviceCtx, output, dxgi.DXGI_FORMAT_R8G8B8A8_UNORM)
}
