package d3d9

import (
	"fmt"
	"unsafe"
)

// Render targets: SetRenderTarget(0, surface) queues a target switch; the render goroutine draws
// each run of operations into its target. A texture used as a render target keeps its colour
// buffer as its Pixels, so later draws that sample it read what was rendered into it by then
// (the operations run in order). The depth buffer is shared (games bind the same depth surface).
// Only targets the size of the back buffer are drawn; operations aimed at others are dropped
// rather than drawn onto the screen.

type renderTarget struct {
	px  []uint32 // RGBA texels, red in the low byte (the texture layout)
	pix []byte   // the same memory as framebuffer bytes
}

// setRenderTarget handles SetRenderTarget(0, surf).
func (s *Subsystem) setRenderTarget(surf uint32) {
	var op queuedTri
	op.rtOp = true
	if s.gpuFrame {
		// The GPU path draws into targets of any size.
		var desc *BufferDesc
		if surf != 0 && surf != s.defaultBackBuf {
			desc = s.Resources[surf]
			if desc != nil {
				desc.IsRT = true
			}
		}
		if desc != s.curTargetDesc || len(s.frameTris) == 0 {
			s.curTargetDesc = desc
			op.rtDesc = desc
			s.frameTris = append(s.frameTris, op)
		}
		return
	}
	if surf != 0 && surf != s.defaultBackBuf {
		desc := s.Resources[surf]
		switch {
		case desc == nil || desc.Width != ScreenWidth || desc.Height != ScreenHeight:
			op.rtSkip = true
			if !s.loggedRTSize && desc != nil {
				s.loggedRTSize = true
				fmt.Printf("[D3D9] render target %dx%d not supported (only %dx%d): its draws are skipped\n",
					desc.Width, desc.Height, ScreenWidth, ScreenHeight)
			}
		default:
			op.rtPix = s.renderTargetFor(desc).pix
		}
	}
	if op.rtSkip == s.curTargetSkip && sameSlice(op.rtPix, s.curTargetPix) {
		return
	}
	s.curTargetSkip, s.curTargetPix = op.rtSkip, op.rtPix
	s.frameTris = append(s.frameTris, op)
}

func sameSlice(a, b []byte) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// renderTargetFor returns the host colour buffer of a screen-sized surface or texture, which from
// then on is the texture's Pixels.
func (s *Subsystem) renderTargetFor(desc *BufferDesc) *renderTarget {
	rt := s.renderTargets[desc]
	if rt == nil {
		rt = &renderTarget{px: make([]uint32, ScreenWidth*ScreenHeight)}
		rt.pix = unsafe.Slice((*byte)(unsafe.Pointer(&rt.px[0])), len(rt.px)*4)
		s.renderTargets[desc] = rt
		desc.Pixels = rt.px
		desc.IsRT = true
	}
	return rt
}

// stretchRect handles StretchRect(src, srcRect, dst, dstRect, filter) between screen-sized surfaces
// (whole surfaces): a copy queued in order with the draws.
func (s *Subsystem) stretchRect(src, dst uint32) {
	sd, dd := s.Resources[src], s.Resources[dst]
	if s.gpuFrame {
		op := queuedTri{copyOp: true, copySrcDesc: sd, copyDstDesc: dd}
		if src == s.defaultBackBuf {
			op.copySrcDesc = nil
		}
		if dst == s.defaultBackBuf {
			op.copyDstDesc = nil
		}
		for _, d := range []*BufferDesc{op.copySrcDesc, op.copyDstDesc} {
			if d != nil {
				d.IsRT = true
			}
		}
		s.frameTris = append(s.frameTris, op)
		return
	}
	if src == s.defaultBackBuf {
		sd = nil
	} else if sd == nil || sd.Width != ScreenWidth || sd.Height != ScreenHeight {
		return
	}
	if dd == nil || dst == s.defaultBackBuf || dd.Width != ScreenWidth || dd.Height != ScreenHeight {
		return
	}
	op := queuedTri{copyOp: true, copyDst: s.renderTargetFor(dd).pix}
	if sd != nil {
		op.copySrc = s.renderTargetFor(sd).pix
	}
	s.frameTris = append(s.frameTris, op)
}
