//go:build windows

package main

// The tray icon and starting with Windows. Wails v2 has no tray, so the icon
// is a small hidden window and a message loop of our own, which is all the
// Windows notification area needs: add an icon, answer its clicks, put it
// back if Explorer restarts.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const backgroundFlag = "--background"

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassEx     = user32.NewProc("RegisterClassExW")
	pCreateWindowEx      = user32.NewProc("CreateWindowExW")
	pDefWindowProc       = user32.NewProc("DefWindowProcW")
	pDestroyWindow       = user32.NewProc("DestroyWindow")
	pGetMessage          = user32.NewProc("GetMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessage     = user32.NewProc("DispatchMessageW")
	pPostMessage         = user32.NewProc("PostMessageW")
	pPostQuitMessage     = user32.NewProc("PostQuitMessage")
	pRegisterWindowMsg   = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	pAppendMenu          = user32.NewProc("AppendMenuW")
	pTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	pDestroyMenu         = user32.NewProc("DestroyMenu")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pGetCursorPos        = user32.NewProc("GetCursorPos")
	pDestroyIcon         = user32.NewProc("DestroyIcon")
	pShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	pExtractIconEx       = shell32.NewProc("ExtractIconExW")
	pGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull            = 0x0000
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmLButtonUp       = 0x0202
	wmRButtonUp       = 0x0205
	wmLButtonDblClk   = 0x0203
	wmTray            = 0x8000 + 1

	nimAdd    = 0
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	mfString    = 0x0
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmReturnCmd   = 0x100

	idOpen = 1
	idQuit = 2
)

// notifyIconData is NOTIFYICONDATAW (Vista and later); field order and sizes
// follow the C struct, and Go's alignment rules give the same layout.
type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeout         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         windows.GUID
	hBalloonIcon     uintptr
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type point struct{ x, y int32 }

// trayHandlers are what the tray icon does.
type trayHandlers struct {
	open    func() // show the window
	quit    func() // close the app for real
	sessEnd func() // Windows is signing out or shutting down
}

// startTray puts the icon in the notification area and returns a function
// that removes it. It returns an error when the icon couldn't be created, so
// the caller can keep the window in the taskbar instead of hiding it.
func startTray(h trayHandlers) (stop func(), err error) {
	type result struct {
		hwnd uintptr
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		// The window and its message loop must live on one thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		exe, _ := os.Executable()
		var small uintptr
		exe16, _ := windows.UTF16PtrFromString(exe)
		pExtractIconEx.Call(uintptr(unsafe.Pointer(exe16)), 0, 0, uintptr(unsafe.Pointer(&small)), 1)

		taskbarCreated, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("TaskbarCreated"))))

		var hwnd uintptr
		addIcon := func() bool {
			nid := notifyIconData{
				cbSize: uint32(unsafe.Sizeof(notifyIconData{})), hWnd: hwnd, uID: 1,
				uFlags: nifMessage | nifIcon | nifTip, uCallbackMessage: wmTray, hIcon: small,
			}
			copy(nid.szTip[:], windows.StringToUTF16("AltNet Relay"))
			r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
			return r != 0
		}
		removeIcon := func() {
			nid := notifyIconData{cbSize: uint32(unsafe.Sizeof(notifyIconData{})), hWnd: hwnd, uID: 1}
			pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
		}
		showMenu := func() {
			menu, _, _ := pCreatePopupMenu.Call()
			if menu == 0 {
				return
			}
			defer pDestroyMenu.Call(menu)
			pAppendMenu.Call(menu, mfString, idOpen, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Open AltNet Relay"))))
			pAppendMenu.Call(menu, mfSeparator, 0, 0)
			pAppendMenu.Call(menu, mfString, idQuit, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Quit"))))
			var pt point
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
			// The menu closes when the user clicks elsewhere only if our
			// window is in front first; this is the documented way.
			pSetForegroundWindow.Call(hwnd)
			cmd, _, _ := pTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd, uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
			pPostMessage.Call(hwnd, wmNull, 0, 0)
			switch cmd {
			case idOpen:
				h.open()
			case idQuit:
				h.quit()
			}
		}

		proc := syscall.NewCallback(func(w uintptr, m uint32, wp, lp uintptr) uintptr {
			switch {
			case m == wmTray:
				switch uint32(lp) {
				case wmLButtonUp, wmLButtonDblClk:
					h.open()
				case wmRButtonUp:
					showMenu()
				}
				return 0
			case m == wmQueryEndSession:
				// Signing out or shutting down: the window must be allowed to
				// close, or Windows waits on us.
				h.sessEnd()
				return 1
			case m == wmEndSession:
				if wp != 0 {
					removeIcon()
				}
				return 0
			case uintptr(m) == taskbarCreated && taskbarCreated != 0:
				addIcon() // Explorer restarted: its notification area is empty again
				return 0
			case m == wmDestroy:
				removeIcon()
				pPostQuitMessage.Call(0)
				return 0
			}
			r, _, _ := pDefWindowProc.Call(w, uintptr(m), wp, lp)
			return r
		})

		hInst, _, _ := pGetModuleHandle.Call(0)
		className, _ := windows.UTF16PtrFromString("AltNetRelayTray")
		wc := wndClassEx{cbSize: uint32(unsafe.Sizeof(wndClassEx{})), lpfnWndProc: proc, hInstance: hInst, lpszClassName: className}
		if r, _, e := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			ready <- result{err: fmt.Errorf("tray window class: %w", e)}
			return
		}
		// A hidden top-level window (not "message only"): only top-level
		// windows are told that Windows is signing out.
		hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
		if hwnd == 0 {
			ready <- result{err: errors.New("tray window couldn't be created")}
			return
		}
		if !addIcon() {
			pDestroyWindow.Call(hwnd)
			ready <- result{err: errors.New("Windows wouldn't add the tray icon")}
			return
		}
		ready <- result{hwnd: hwnd}

		var m msg
		for {
			r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
		if small != 0 {
			pDestroyIcon.Call(small)
		}
	}()

	res := <-ready
	if res.err != nil {
		return nil, res.err
	}
	return func() { pPostMessage.Call(res.hwnd, wmClose, 0, 0) }, nil
}

// ─── starting with Windows ───

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "AltNetRelay"

func startupAvailable() bool { return true }

// startupEnabled reports whether Windows starts the app at sign-in. The
// registry is the truth: the installer, this window and the user (Task
// Manager's Startup tab) can all change it.
func startupEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return err == nil && v != ""
}

// setStartup makes Windows start the app, without a window, when its user
// signs in -- or stops it. Per user, no administrator needed.
func setStartup(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValue, `"`+exe+`" `+backgroundFlag)
}

// nameToasts makes Windows head its notifications "AltNet Relay" rather than
// the program's file name.
func nameToasts() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+filepath.Base(exe), registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("DisplayName", "AltNet Relay")
}
