// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package notify

// The Windows tray: one notification-area icon owned by berth notify in
// the user's session. Its balloons are Windows' own toasts, its tooltip is
// the latest event, and its menu opens the harbor, mutes the toasts, lists
// the recent events and quits. Plain Win32 through x/sys/windows: no cgo
// and no PowerShell.

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed berth.ico
var icon []byte

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	pRegisterClassEx       = user32.NewProc("RegisterClassExW")
	pCreateWindowEx        = user32.NewProc("CreateWindowExW")
	pDefWindowProc         = user32.NewProc("DefWindowProcW")
	pDestroyWindow         = user32.NewProc("DestroyWindow")
	pGetMessage            = user32.NewProc("GetMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessage       = user32.NewProc("DispatchMessageW")
	pPostMessage           = user32.NewProc("PostMessageW")
	pPostQuitMessage       = user32.NewProc("PostQuitMessage")
	pLoadImage             = user32.NewProc("LoadImageW")
	pCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	pAppendMenu            = user32.NewProc("AppendMenuW")
	pDestroyMenu           = user32.NewProc("DestroyMenu")
	pTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	pGetCursorPos          = user32.NewProc("GetCursorPos")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	pShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	errTrayIcon            = errors.New("the notification area refused the icon")
	taskbarCreated         uintptr // explorer restarted: add the icon again
)

const (
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmNull        = 0x0000
	wmContextMenu = 0x007B
	wmLButtonUp   = 0x0202
	wmApp         = 0x8000
	wmTray        = wmApp + 1

	ninBalloonUserClick = 0x0405

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage  = 0x01
	nifIcon     = 0x02
	nifTip      = 0x04
	nifInfo     = 0x10
	nifShowTip  = 0x80
	niifInfo    = 0x01
	niifError   = 0x03
	niifNoSound = 0x10

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	imageIcon      = 1
	lrLoadFromFile = 0x0010
	lrDefaultSize  = 0x0040

	menuOpen = 1
	menuMute = 2
	menuQuit = 3
)

type notifyIconData struct {
	Size            uint32
	Wnd             windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	Wnd     windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

// tray is the icon's state. The window procedure runs on the locked
// thread; Send runs on the follow goroutine; mu covers what both touch.
type tray struct {
	harbor string
	wnd    windows.HWND
	icon   windows.Handle

	mu     sync.Mutex
	muted  bool
	recent Recent
}

// Serve shows the notes follow sends from a tray icon until ctx ends or
// the person picks Quit.
func Serve(ctx context.Context, harbor string, follow func(Sender)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	t := &tray{harbor: harbor, recent: Recent{Max: 10}}
	if err := t.create(); err != nil {
		return err
	}
	go follow(t)
	go func() {
		<-ctx.Done()
		_, _, _ = pPostMessage.Call(uintptr(t.wnd), wmClose, 0, 0)
	}()
	var m msg
	for {
		r, _, err := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("tray message loop: %w", err)
		case 0:
			return nil
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *tray) create() error {
	path := filepath.Join(os.TempDir(), "berth-notify.ico")
	if err := os.WriteFile(path, icon, 0o644); err != nil {
		return fmt.Errorf("tray icon: %w", err)
	}
	h, _, err := pLoadImage.Call(0, uintptr(unsafe.Pointer(utf16p(path))),
		imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
	if h == 0 {
		return fmt.Errorf("tray icon: %w", err)
	}
	t.icon = windows.Handle(h)

	name := utf16p("BerthNotifyTray")
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		return fmt.Errorf("tray window: %w", err)
	}
	wc := wndClassEx{WndProc: windows.NewCallback(t.proc), Instance: inst, ClassName: name}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("tray window class: %w", err)
	}
	wnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if wnd == 0 {
		return fmt.Errorf("tray window: %w", err)
	}
	t.wnd = windows.HWND(wnd)
	taskbarCreated, _, _ = pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(utf16p("TaskbarCreated"))))
	return t.add()
}

// add puts the icon in the notification area, with version 4 callbacks.
func (t *tray) add() error {
	d := t.data()
	d.Flags = nifMessage | nifIcon | nifTip | nifShowTip
	d.CallbackMessage = wmTray
	d.Icon = t.icon
	copyUTF16(d.Tip[:], "Berth")
	if r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(d))); r == 0 {
		return errTrayIcon
	}
	d.Version = 4
	_, _, _ = pShellNotifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(d)))
	return nil
}

func (t *tray) data() *notifyIconData {
	d := &notifyIconData{Wnd: t.wnd, ID: 1}
	d.Size = uint32(unsafe.Sizeof(*d))
	return d
}

// Send records n, shows it as the tooltip, and as a toast unless muted.
func (t *tray) Send(n Note) error {
	t.mu.Lock()
	t.recent.Add(n)
	muted := t.muted
	t.mu.Unlock()
	d := t.data()
	d.Flags = nifTip | nifShowTip
	copyUTF16(d.Tip[:], "Berth: "+n.Title)
	if !muted {
		d.Flags |= nifInfo
		copyUTF16(d.InfoTitle[:], n.Title)
		copyUTF16(d.Info[:], n.Body)
		d.InfoFlags = niifInfo | niifNoSound
		if n.Level == Error {
			d.InfoFlags = niifError
		}
	}
	if r, _, _ := pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(d))); r == 0 {
		return errTrayIcon
	}
	return nil
}

func (t *tray) proc(wnd, message, wparam, lparam uintptr) uintptr {
	switch {
	case message == wmTray:
		switch lparam & 0xFFFF {
		case wmContextMenu, wmLButtonUp:
			t.menu()
		case ninBalloonUserClick:
			t.open()
		}
		return 0
	case message == taskbarCreated && taskbarCreated != 0:
		_ = t.add()
		return 0
	case message == wmClose:
		_, _, _ = pDestroyWindow.Call(wnd)
		return 0
	case message == wmDestroy:
		_, _, _ = pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(t.data())))
		_, _, _ = pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(wnd, message, wparam, lparam)
	return r
}

// menu builds the popup fresh each time, so the recent events are current.
func (t *tray) menu() {
	t.mu.Lock()
	muted := t.muted
	notes := append([]Note(nil), t.recent.Notes()...)
	t.mu.Unlock()

	m, _, _ := pCreatePopupMenu.Call()
	defer func() { _, _, _ = pDestroyMenu.Call(m) }()
	sub, _, _ := pCreatePopupMenu.Call()
	if len(notes) == 0 {
		appendMenu(sub, mfString|mfGrayed, 0, "Nothing yet")
	}
	for _, n := range notes {
		appendMenu(sub, mfString|mfGrayed, 0, n.Title)
	}
	appendMenu(m, mfString, menuOpen, "Open harbor")
	show := uintptr(mfString | mfChecked)
	if muted {
		show = mfString
	}
	appendMenu(m, show, menuMute, "Show notifications")
	appendMenu(m, mfPopup, sub, "Recent events")
	appendMenu(m, mfSeparator, 0, "")
	appendMenu(m, mfString, menuQuit, "Quit")

	var pt struct{ X, Y int32 }
	_, _, _ = pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = pSetForegroundWindow.Call(uintptr(t.wnd))
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmRightButton|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(t.wnd), 0)
	_, _, _ = pPostMessage.Call(uintptr(t.wnd), wmNull, 0, 0)
	switch cmd {
	case menuOpen:
		t.open()
	case menuMute:
		t.mu.Lock()
		t.muted = !t.muted
		t.mu.Unlock()
	case menuQuit:
		_, _, _ = pPostMessage.Call(uintptr(t.wnd), wmClose, 0, 0)
	}
}

func (t *tray) open() {
	_ = windows.ShellExecute(0, utf16p("open"), utf16p(t.harbor), nil, nil, windows.SW_SHOWNORMAL)
}

func appendMenu(m, flags, id uintptr, text string) {
	_, _, _ = pAppendMenu.Call(m, flags, id, uintptr(unsafe.Pointer(utf16p(text))))
}

// utf16p is s as a Win32 string; the texts here hold no NUL.
func utf16p(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// copyUTF16 writes s into a fixed Win32 text field, cut to fit with its
// terminating zero.
func copyUTF16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
