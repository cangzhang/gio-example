// SPDX-License-Identifier: Unlicense OR MIT

// +build darwin windows

package main

// This program demonstrates the use of a custom OpenGL ES context with
// app.Window. It is similar to the GLFW example, but uses Gio's window
// implementation instead of the one in GLFW.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"gioui.org/app"
	"gioui.org/gpu"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"gioui.org/font/gofont"
)

/*
#cgo CFLAGS: -DEGL_NO_X11
#cgo LDFLAGS: -lEGL -lGLESv2

#include <EGL/egl.h>
#include <GLES2/gl2.h>

*/
import "C"

type eglContext struct {
	disp C.EGLDisplay
	ctx  C.EGLContext
	surf C.EGLSurface
}

const (
	// needDepthBuffer must be true when the program needs a depth buffer, or
	// when using the old non-compute Gio renderer.
	needDepthBuffer = true
)

func main() {
	go func() {
		// Set CustomRenderer so we can provide our own rendering context.
		w := app.NewWindow(app.CustomRenderer(true))
		if err := loop(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

var button widget.Clickable

func loop(w *app.Window) error {
	// OpenGL stores the current context in thread local storage.
	runtime.LockOSThread()

	th := material.NewTheme(gofont.Collection())
	var ops op.Ops
	var (
		ctx    *eglContext
		gioCtx gpu.GPU
	)
	for e := range w.Events() {
		switch e := e.(type) {
		case app.ViewEvent:
			w.Run(func() {
				if gioCtx != nil {
					gioCtx.Release()
					gioCtx = nil
				}
				if ctx != nil {
					ctx.Release()
					ctx = nil
				}
				view := nativeViewFor(e)
				var nilv C.EGLNativeWindowType
				if view == nilv {
					return
				}
				c, err := createContext(view)
				if err != nil {
					log.Fatal(err)
				}
				ctx = c
				if ok := C.eglMakeCurrent(ctx.disp, ctx.surf, ctx.surf, ctx.ctx); ok != C.EGL_TRUE {
					err := fmt.Errorf("eglMakeCurrent failed (%#x)", C.eglGetError())
					log.Fatal(err)
				}
				glGetString := func(e C.GLenum) string {
					return C.GoString((*C.char)(unsafe.Pointer(C.glGetString(e))))
				}
				fmt.Printf("GL_VERSION: %s\nGL_RENDERER: %s\n", glGetString(C.GL_VERSION), glGetString(C.GL_RENDERER))
				gioCtx, err = gpu.New(gpu.OpenGL{ES: true})
				if err != nil {
					log.Fatal(err)
				}
			})
		case system.DestroyEvent:
			return e.Err
		case system.FrameEvent:
			if gioCtx == nil {
				break
			}
			// Build ops.
			gtx := layout.NewContext(&ops, e)
			// Catch pointer events not hitting UI.
			types := pointer.Move | pointer.Press | pointer.Release
			pointer.InputOp{Tag: w, Types: types}.Add(gtx.Ops)
			for _, e := range gtx.Events(w) {
				log.Println("Event:", e)
			}
			drawUI(th, gtx)
			w.Run(func() {
				if ok := C.eglMakeCurrent(ctx.disp, ctx.surf, ctx.surf, ctx.ctx); ok != C.EGL_TRUE {
					err := fmt.Errorf("eglMakeCurrent failed (%#x)", C.eglGetError())
					log.Fatal(err)
				}
				// Trigger window resize detection in ANGLE.
				C.eglWaitClient()
				// Draw custom OpenGL content.
				drawGL()

				// Render drawing ops.
				gioCtx.Collect(e.Size, gtx.Ops)
				gioCtx.Frame()

				if ok := C.eglSwapBuffers(ctx.disp, ctx.surf); ok != C.EGL_TRUE {
					log.Fatal(fmt.Errorf("swap failed: %v", C.eglGetError()))
				}
			})

			// Process non-drawing ops.
			e.Frame(gtx.Ops)
		}
	}
	return nil
}

func drawGL() {
	C.glClearColor(.5, .5, 0, 1)
	C.glClear(C.GL_COLOR_BUFFER_BIT | C.GL_DEPTH_BUFFER_BIT)
}

func drawUI(th *material.Theme, gtx layout.Context) layout.Dimensions {
	return layout.Center.Layout(gtx,
		material.Button(th, &button, "Button").Layout,
	)
}

func createContext(view C.EGLNativeWindowType) (*eglContext, error) {
	disp := C.eglGetDisplay(C.EGL_DEFAULT_DISPLAY)
	if disp == 0 {
		return nil, fmt.Errorf("eglGetPlatformDisplay failed: 0x%x", C.eglGetError())
	}
	var major, minor C.EGLint
	if ok := C.eglInitialize(disp, &major, &minor); ok != C.EGL_TRUE {
		return nil, fmt.Errorf("eglInitialize failed: 0x%x", C.eglGetError())
	}
	exts := strings.Split(C.GoString(C.eglQueryString(disp, C.EGL_EXTENSIONS)), " ")
	srgb := hasExtension(exts, "EGL_KHR_gl_colorspace")
	attribs := []C.EGLint{
		C.EGL_RENDERABLE_TYPE, C.EGL_OPENGL_ES2_BIT,
		C.EGL_SURFACE_TYPE, C.EGL_WINDOW_BIT,
		C.EGL_BLUE_SIZE, 8,
		C.EGL_GREEN_SIZE, 8,
		C.EGL_RED_SIZE, 8,
		C.EGL_CONFIG_CAVEAT, C.EGL_NONE,
	}
	if srgb {
		// Some drivers need alpha for sRGB framebuffers to work.
		attribs = append(attribs, C.EGL_ALPHA_SIZE, 8)
	}
	if needDepthBuffer {
		attribs = append(attribs, C.EGL_DEPTH_SIZE, 16)
	}
	attribs = append(attribs, C.EGL_NONE)
	var (
		cfg     C.EGLConfig
		numCfgs C.EGLint
	)
	if ok := C.eglChooseConfig(disp, &attribs[0], &cfg, 1, &numCfgs); ok != C.EGL_TRUE {
		return nil, fmt.Errorf("eglChooseConfig failed: 0x%x", C.eglGetError())
	}
	if numCfgs == 0 {
		supportsNoCfg := hasExtension(exts, "EGL_KHR_no_config_context")
		if !supportsNoCfg {
			return nil, errors.New("eglChooseConfig returned no configs")
		}
	}
	ctxAttribs := []C.EGLint{
		C.EGL_CONTEXT_CLIENT_VERSION, 3,
		C.EGL_NONE,
	}
	ctx := C.eglCreateContext(disp, cfg, nil, &ctxAttribs[0])
	if ctx == nil {
		return nil, fmt.Errorf("eglCreateContext failed: 0x%x", C.eglGetError())
	}
	var surfAttribs []C.EGLint
	if srgb {
		surfAttribs = append(surfAttribs, C.EGL_GL_COLORSPACE, C.EGL_GL_COLORSPACE_SRGB)
	}
	surfAttribs = append(surfAttribs, C.EGL_NONE)
	surf := C.eglCreateWindowSurface(disp, cfg, view, &surfAttribs[0])
	if surf == nil {
		return nil, fmt.Errorf("eglCreateWindowSurface failed (0x%x)", C.eglGetError())
	}
	return &eglContext{disp: disp, ctx: ctx, surf: surf}, nil
}

func (c *eglContext) Release() {
	if c.ctx != nil {
		C.eglDestroyContext(c.disp, c.ctx)
	}
	if c.surf != nil {
		C.eglDestroySurface(c.disp, c.surf)
	}
	*c = eglContext{}
}

func hasExtension(exts []string, ext string) bool {
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}
