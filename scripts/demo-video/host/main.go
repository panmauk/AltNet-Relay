// A bare window around WebView2 with a DevTools port, so a script can drive
// the page and record it. Recording only -- not part of the app.
//
//	host <url> <port> <datadir> <x> <y> <w> <h>
package main

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	pRegisterClassEx = user32.NewProc("RegisterClassExW")
	pCreateWindowEx  = user32.NewProc("CreateWindowExW")
	pDefWindowProc   = user32.NewProc("DefWindowProcW")
	pGetMessage      = user32.NewProc("GetMessageW")
	pTranslate       = user32.NewProc("TranslateMessage")
	pDispatch        = user32.NewProc("DispatchMessageW")
	pPostQuit        = user32.NewProc("PostQuitMessage")
	pDpiCtx          = user32.NewProc("SetProcessDpiAwarenessContext")
)

type wndClassEx struct {
	size, style            uint32
	wndProc                uintptr
	clsExtra, wndExtra     int32
	instance, icon, cursor uintptr
	background             uintptr
	menuName, className    *uint16
	iconSm                 uintptr
}

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	x, y    int32
	priv    uint32
}

var chromium *edge.Chromium

func wndProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	switch m {
	case 0x0005: // WM_SIZE
		if chromium != nil {
			chromium.Resize()
		}
	case 0x0002: // WM_DESTROY
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), w, l)
	return r
}

func atoi(s string) uintptr { n, _ := strconv.Atoi(s); return uintptr(n) }

func main() {
	runtime.LockOSThread()
	url, port, data := os.Args[1], os.Args[2], os.Args[3]
	x, y, w, h := atoi(os.Args[4]), atoi(os.Args[5]), atoi(os.Args[6]), atoi(os.Args[7])

	// Per-monitor DPI aware v2, so sizes are real pixels.
	pDpiCtx.Call(^uintptr(3))

	cls, _ := windows.UTF16PtrFromString("RelayRecorderHost")
	title, _ := windows.UTF16PtrFromString("AltNet Relay (recording)")
	wc := wndClassEx{wndProc: syscall.NewCallback(wndProc), className: cls}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	const style = 0x00CF0000 | 0x10000000 // WS_OVERLAPPEDWINDOW | WS_VISIBLE
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
		style, x, y, w, h, 0, 0, 0, 0)

	chromium = edge.NewChromium()
	chromium.DataPath = data
	chromium.AdditionalBrowserArgs = []string{
		"--remote-debugging-port=" + port,
		"--disable-features=CalculateNativeWinOcclusion",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
	}
	chromium.SetGlobalPermission(edge.CoreWebView2PermissionStateAllow)
	if !chromium.Embed(hwnd) {
		os.Exit(1)
	}
	chromium.SetBackgroundColour(6, 10, 24, 255)
	chromium.Resize()
	chromium.Navigate(url)

	var m msgT
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			return
		}
		pTranslate.Call(uintptr(unsafe.Pointer(&m)))
		pDispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}
