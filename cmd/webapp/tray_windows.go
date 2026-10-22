//go:build windows

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// tray_windows.go Windows 托盘图标（通知区域）。
//
// 为什么要有它：本程序是**后台服务**，Windows 下由启动脚本以 -WindowStyle Hidden
// 拉起 —— 桌面上一闪而过、之后什么都没有。用户既不知道它是否还活着，也没有任何
// 办法关掉它（只能去任务管理器里找 webapp.exe）。这不是小毛病：一个"看不见、
// 关不掉"的常驻进程，正是用户对本地服务失去控制感的源头。
//
// 现在：右下角常驻一个图标 —— 左键打开控制台，右键给菜单（打开控制台 / 打开
// 数据目录 / 关于 / 退出），退出走的是与 SIGTERM **完全相同**的优雅停机路径。
//
// 为什么只依赖标准库：托盘只需要 shell32/user32/gdi32 的十来个函数，
// 且本仓库刻意保持零 GUI 依赖（见 term_windows.go 同样的取舍 —— 不引
// golang.org/x/sys）。Icon 也在内存里画（见 trayIconBits），不附带二进制资源，
// 于是 `go build` 出来的单个 exe 仍然是全部分发物。
//
// 非 Windows 由 tray_other.go 提供空实现（Linux/macOS 常以 systemd/launchd
// 托管，本就没有"右下角"）。
package main

import (
	"log"
	"math"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------- 常量

const (
	// 窗口消息
	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmCommand      = 0x0111
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmTrayCallback = 0x0400 + 1 // WM_USER+1：托盘图标回调
	wmQuitTray     = 0x0400 + 2 // WM_USER+2：内部「请退出消息循环」

	// Shell_NotifyIcon 的动作
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	// NOTIFYICONDATA.uFlags
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	// 气泡提示样式（dwInfoFlags）
	niifInfo = 0x00000001

	// 菜单
	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfDisabled  = 0x00000002
	mfGrayed    = 0x00000001

	tpmLeftButton  = 0x0000
	tpmRightButton = 0x0002
	tpmLeftAlign   = 0x0000
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100
	tpmNonotify    = 0x0080

	swShowNormal = 1
)

// 托盘菜单项 ID。
const (
	menuOpenConsole = 1
	menuOpenDataDir = 2
	menuAbout       = 3
	menuExit        = 4
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx    = user32.NewProc("RegisterClassExW")
	procCreateWindowEx     = user32.NewProc("CreateWindowExW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procDefWindowProc      = user32.NewProc("DefWindowProcW")
	procGetMessage         = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessage    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procPostMessage        = user32.NewProc("PostMessageW")
	procShellNotifyIcon    = shell32.NewProc("Shell_NotifyIconW")
	procShellExecute       = shell32.NewProc("ShellExecuteW")
	procCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenu         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	procDestroyMenu        = user32.NewProc("DestroyMenu")
	procGetCursorPos       = user32.NewProc("GetCursorPos")
	procSetForegroundWin   = user32.NewProc("SetForegroundWindow")
	procGetDC              = user32.NewProc("GetDC")
	procReleaseDC          = user32.NewProc("ReleaseDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procCreateIconIndirect = user32.NewProc("CreateIconIndirect")
	procDestroyIcon        = user32.NewProc("DestroyIcon")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procGetModuleHandle    = kernel32.NewProc("GetModuleHandleW")
)

// ---------------------------------------------------------------- Win32 结构

// 说明：以下结构体与 C 侧的字段对齐**逐字节核对过**（含指针字段 8 字节对齐带来的
// 隐式填充）。这类结构写错一个字段宽度不会报错，只会得到一个"偶发失效"的托盘，
// 因此每个结构体都标了总大小，改动时必须同步更新注释里的字节数。

// wndClassEx == WNDCLASSEXW（64 位下 80 字节）。
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

// notifyIconData == NOTIFYICONDATAW（64 位下 976 字节，V3/V4 布局）。
//
// 用 V3 布局而不是只填前几个字段：cbSize 决定 Windows 按哪一版解释结构体，
// 传小了会拿不到气泡提示（uVersion/szInfo 那一段被当成不存在）。
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
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

// iconInfo == ICONINFO（fIcon/xHotspot/yHotspot + 两张位图）。
type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

// bitmapInfoHeader == BITMAPINFOHEADER（40 字节）。
type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type point struct{ x, y int32 }

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// ---------------------------------------------------------------- 对外 API

var (
	trayMu   sync.Mutex
	trayHwnd uintptr
	trayMenu uintptr
	trayIcon uintptr
	trayOpts trayOptions
	trayURL  string
)

// startTray 拉起托盘（非阻塞）。失败只记日志、不影响服务本身 ——
// 托盘是**锦上添花**，不能因为没有桌面（服务会话 / 远程桌面断开 / 无交互登录）
// 就把整个服务拖死。
func startTray(opts trayOptions) {
	trayMu.Lock()
	if trayHwnd != 0 { // 已起过：只更新参数，不重复建窗口
		trayOpts = opts
		trayMu.Unlock()
		return
	}
	trayOpts = opts
	trayURL = "http://" + opts.addr
	trayMu.Unlock()

	go trayLoop()
}

// stopTray 收掉图标并退出消息循环。服务停止/退出路径都会走到它，
// 否则进程没了、图标还留在托盘里（要等鼠标划过才消失 —— 用户会以为"没关掉"）。
func stopTray() {
	trayMu.Lock()
	hwnd := trayHwnd
	trayMu.Unlock()
	if hwnd == 0 {
		return
	}
	// 用 PostMessage 而不是直接 DestroyWindow：窗口属于托盘线程，
	// 跨线程销毁窗口是典型的使用错误（消息可能正在派发中）。
	procPostMessage.Call(hwnd, wmQuitTray, 0, 0)
}

// ---------------------------------------------------------------- 消息循环

// trayLoop 托盘线程主体。
//
// ⚠️ 必须 LockOSThread：Win32 的窗口与其创建线程**绑定**（消息只投递给创建它的
// 线程，线程一退出窗口即销毁）。Go 的 goroutine 会在 M 之间迁移，若不锁线程，
// 这个窗口会在某次调度后彻底失联 —— 表现是"图标在，点了没反应"。
func trayLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className, err := syscall.UTF16PtrFromString("wb2api_tray_wnd")
	if err != nil {
		log.Printf("托盘未启用：%v", err)
		return
	}
	hInst, _, _ := procGetModuleHandle.Call(0)

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     hInst,
		lpszClassName: className,
	}
	if r1, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r1 == 0 {
		log.Printf("托盘未启用：注册窗口类失败（无桌面会话时会这样，不影响服务）")
		return
	}

	title, _ := syscall.UTF16PtrFromString("workbuddy2api")
	// 刻意创建**真实的隐藏窗口**而不是 HWND_MESSAGE 消息窗口：
	// 弹出菜单要求窗口可被前置（SetForegroundWindow），消息窗口做不到，
	// 表现是"菜单弹出来后点别处不消失"。这个窗口永不 ShowWindow，用户看不见。
	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		log.Printf("托盘未启用：创建窗口失败（无桌面会话时会这样，不影响服务）")
		return
	}

	icon := createTrayIcon()
	if icon == 0 {
		log.Printf("托盘未启用：创建图标失败")
		procDestroyWindow.Call(hwnd)
		return
	}

	trayMu.Lock()
	trayHwnd = hwnd
	trayIcon = icon
	opts := trayOpts
	trayMu.Unlock()

	if !addTrayIcon(hwnd, icon, opts) {
		procDestroyIcon.Call(icon)
		procDestroyWindow.Call(hwnd)
		trayMu.Lock()
		trayHwnd = 0
		trayIcon = 0
		trayMu.Unlock()
		log.Printf("托盘未启用：Shell_NotifyIcon 失败（资源管理器未就绪时会这样，不影响服务）")
		return
	}
	log.Printf("托盘图标已启用：左键打开控制台，右键菜单可「退出」（%s）", trayURL)

	for {
		var m msg
		// GetMessage 返回 0=收到 WM_QUIT，-1=出错；两者都要跳出，否则会变成
		// 一个吃满 CPU 的死循环（这是 Win32 消息循环的经典坑）。
		r1, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r1 == 0 || int32(r1) == -1 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	removeTrayIcon(hwnd)
	trayMu.Lock()
	trayHwnd = 0
	trayIcon = 0
	trayMu.Unlock()
}

// trayWndProc 窗口过程。返回的 uintptr 是 LRESULT。
func trayWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uint32(uMsg) {
	case wmLButtonUp:
		openWithShell(trayURL)
		return 0
	case wmRButtonUp:
		showTrayMenu(hwnd)
		return 0
	case wmCommand:
		handleTrayCommand(uint32(wParam) & 0xffff)
		return 0
	case wmQuitTray, wmClose, wmDestroy:
		// 统一在这里收尾：图标、菜单、窗口、消息循环。
		removeTrayIcon(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	case wmTrayCallback:
		switch uint32(lParam) {
		case wmRButtonUp, 0x0204 /* WM_RBUTTONDOWN */ :
			showTrayMenu(hwnd)
		case wmLButtonUp:
			openWithShell(trayURL)
		}
		return 0
	}
	r1, _, _ := procDefWindowProc.Call(hwnd, uMsg, wParam, lParam)
	return r1
}

// ---------------------------------------------------------------- 图标增删

// addTrayIcon 加图标，并顺带弹一次气泡 —— 这是"服务真的起来了"的唯一可见信号
// （启动脚本把窗口藏起来了，日志没人看）。
func addTrayIcon(hwnd, icon uintptr, opts trayOptions) bool {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTrayCallback
	nid.hIcon = icon
	copy(nid.szTip[:], utf16Of(clipText("workbuddy2api "+opts.version+" · "+opts.addr, 127)))

	// 首次提示（NIF_INFO）：告诉用户"它在后台，从这里可以关掉它"。
	// 只弹这一次 —— 每次启动都弹气泡会变成骚扰，用户第一反应就是关通知。
	nid.uFlags |= nifInfo
	nid.dwInfoFlags = niifInfo
	copy(nid.szInfo[:], utf16Of(clipText("已在后台运行。右键本图标可打开控制台或退出。", 255)))
	copy(nid.szInfoTitle[:], utf16Of(clipText("workbuddy2api 已启动", 63)))

	r1, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	return r1 != 0
}

func removeTrayIcon(hwnd uintptr) {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

	trayMu.Lock()
	icon := trayIcon
	menu := trayMenu
	trayIcon = 0
	trayMenu = 0
	trayMu.Unlock()
	if icon != 0 {
		procDestroyIcon.Call(icon)
	}
	if menu != 0 {
		procDestroyMenu.Call(menu)
	}
}

// ---------------------------------------------------------------- 右键菜单

func showTrayMenu(hwnd uintptr) {
	trayMu.Lock()
	opts := trayOpts
	trayMu.Unlock()

	if trayMenu == 0 {
		// 菜单**只建一次**：每次右键都建一个、又不销毁，是托盘程序最常见的
		// 句柄泄漏（用户点十次菜单就漏十个 HMENU）。
		h, _, _ := procCreatePopupMenu.Call()
		if h == 0 {
			return
		}
		appendItem(h, mfString, menuOpenConsole, "打开控制台")
		appendItem(h, mfString, menuOpenDataDir, "打开数据目录")
		appendItem(h, mfSeparator, 0, "")
		// 「关于」做成禁用项：菜单里没有"只读文本"这种东西，禁用项既能显示
		// 版本与地址、又天然不可点 —— 比弹 MessageBox 轻，也不打断用户。
		appendItem(h, mfString|mfGrayed|mfDisabled, menuAbout,
			clipText("workbuddy2api "+opts.version+" · "+opts.addr, 60))
		appendItem(h, mfSeparator, 0, "")
		appendItem(h, mfString, menuExit, "退出（结束应用）")
		trayMu.Lock()
		trayMenu = h
		trayMu.Unlock()
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// ⚠️ 这两个调用缺一不可：不 SetForegroundWindow，菜单弹出后**点空白处不消失**
	// （Windows 的经典行为）；不补一个消息，菜单在某些主题下会"粘"在屏幕上。
	procSetForegroundWin.Call(hwnd)
	// 用经典路径：菜单项经 WM_COMMAND 回投本窗口（见 trayWndProc）。
	//
	// 刻意**不用** TPM_RETURNCMD：那条路径下 TrackPopupMenu 直接返回 ID，
	// 看着更省事，但两种回传方式混用会**执行两次**（实测类库里最常见的症状是
	// 「点一次打开控制台，浏览器开了两个标签页」）。只保留一条路径。
	procTrackPopupMenu.Call(
		trayMenu,
		tpmLeftButton|tpmRightButton|tpmLeftAlign|tpmBottomAlign,
		uintptr(int(pt.x)), uintptr(int(pt.y)), 0, hwnd, 0,
	)
	procPostMessage.Call(hwnd, 0, 0, 0) // WM_NULL：确保菜单彻底收掉
}

func appendItem(hMenu uintptr, flags uint32, id uintptr, text string) {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenu.Call(hMenu, uintptr(flags), id, uintptr(unsafe.Pointer(p)))
}

// handleTrayCommand 执行菜单动作。
func handleTrayCommand(id uint32) {
	trayMu.Lock()
	opts := trayOpts
	trayMu.Unlock()
	switch id {
	case menuOpenConsole:
		openWithShell("http://" + opts.addr)
	case menuOpenDataDir:
		if opts.dataDir != "" {
			openWithShell(opts.dataDir)
		}
	case menuExit:
		// 顺序很重要：先收图标（给用户"已经点了、正在关"的即时反馈），
		// 再走优雅停机（可能要等最多 20 秒的在途请求）。
		stopTray()
		if opts.onExit != nil {
			opts.onExit()
		}
	}
}

// openWithShell 用 ShellExecute 打开 URL 或目录（走系统关联程序，
// 不假设浏览器/资源管理器的路径 —— 硬编码 chrome.exe 在换机器时就废了）。
func openWithShell(target string) {
	p, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return
	}
	verb, _ := syscall.UTF16PtrFromString("open")
	procShellExecute.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(p)), 0, 0, swShowNormal)
}

// ---------------------------------------------------------------- 图标画制

// trayIconSize 托盘图标边长。32 是托盘的原生尺寸（16 会被放大后发虚，
// 48 以上在 100% 缩放下反而被缩），取 32 最稳。
const trayIconSize = 32

// createTrayIcon 在内存里画一个图标并返回 HICON。
//
// 为什么手画而不是带一个 .ico：分发物要保持"只有一个 exe"（go:embed 一个二进制
// 图标 + 启动时写临时文件再 LoadImage，会引入临时目录权限与清理问题，而收益只是
// 一个更好看的图标）。这里用"蓝底圆角 + 白色 W"—— 与页面主色一致，缩到 16px
// 也能认出来。
func createTrayIcon() uintptr {
	xor, mask := trayIconBits()
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer procReleaseDC.Call(0, hdc)

	hbmColor := createDIB(hdc, trayIconSize, trayIconSize, 32, xor)
	hbmMask := createDIB(hdc, trayIconSize, trayIconSize, 1, mask)
	if hbmColor == 0 || hbmMask == 0 {
		if hbmColor != 0 {
			procDeleteObject.Call(hbmColor)
		}
		if hbmMask != 0 {
			procDeleteObject.Call(hbmMask)
		}
		return 0
	}

	ii := iconInfo{fIcon: 1, hbmMask: hbmMask, hbmColor: hbmColor}
	icon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	// 位图已经"并进"图标：无论成功与否都要释放自己的句柄，
	// 否则每次启动都漏两张位图（GDI 对象泄漏在长跑进程里是硬伤）。
	procDeleteObject.Call(hbmColor)
	procDeleteObject.Call(hbmMask)
	return icon
}

// createDIB 建一张 DIB 位图并把像素拷进去。
//
// 用 CreateDIBSection 而不是 CreateBitmap：ICONINFO 要求 32bpp 图标的颜色位图
// 必须是 DIB section，用 CreateBitmap 出来的 DDB 在部分系统上会画成全黑。
func createDIB(hdc uintptr, w, h int32, bpp uint16, bits []byte) uintptr {
	bmi := bitmapInfoHeader{
		biSize:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:    w,
		biHeight:   h, // 正 = 自下而上（DIB 约定；与 ICONINFO 一致）
		biPlanes:   1,
		biBitCount: bpp,
	}
	// ppvBits 刻意声明成 unsafe.Pointer 而不是 uintptr：CreateDIBSection 把
	// 像素区地址**写进**这个变量，之后要把它转回指针。若用 uintptr 中转，
	// 就得写 unsafe.Pointer(ppvBits) —— go vet 会直接报 "possible misuse of
	// unsafe.Pointer"（GC 可能在两次转换之间移动/回收对象），且这是真会出事的模式。
	var ppvBits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bmi)),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&ppvBits)),
		0, 0,
	)
	if hbm == 0 || ppvBits == nil {
		return 0
	}
	dst := unsafe.Slice((*byte)(ppvBits), len(bits))
	copy(dst, bits)
	return hbm
}

// trayIconBits 生成 32×32 图标的像素：颜色位图（BGRA，自下而上）与 1bpp 掩码。
//
// 掩码全 0：32bpp 图标的透明由颜色位图自己的 alpha 决定，掩码只在"完全透明"
// 的老式用法里起作用；全 0 的掩码是 32bpp 图标的标准写法。
func trayIconBits() (color []byte, mask []byte) {
	const s = trayIconSize
	color = make([]byte, s*s*4)
	// 1bpp 每行补齐到 4 字节：32px = 32bit = 4B，正好无需额外 padding。
	mask = make([]byte, s*s/8)

	// 圆角矩形底色（品牌蓝 #2F6FED，BGRA 顺序）
	const radius = 7.0
	const (
		b = 0xED
		g = 0x6F
		r = 0x2F
	)
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			if !inRoundedRect(float64(x)+0.5, float64(y)+0.5, 0, 0, s, s, radius) {
				continue
			}
			i := ((s-1-y)*s + x) * 4 // DIB 自下而上
			color[i], color[i+1], color[i+2], color[i+3] = b, g, r, 0xFF
		}
	}

	// 白色 "W"：由 4 段折线 + 描边宽度渲染，而不是逐像素点阵 ——
	// 点阵换个尺寸就要重画一遍，折线改一个参数即可。
	//
	// ⚠️ 描边半宽是**唯一需要小心**的参数：32px 里四条笔画间距只有 4~5px，
	// 半宽到 2.6 时相邻笔画会在中段糊成一条实心横杠（"W" 变成 "▤"）。
	// 取 1.5（笔画约 3px）才能保住中间的空隙。改这个值时务必重跑
	// TestTrayIconBits 里的字形断言。
	type pt struct{ x, y float64 }
	const (
		pad   = 5.0 // 字形外边距
		width = 1.5 // 描边半宽
	)
	inner := float64(s) - 2*pad
	pts := []pt{
		{pad, pad},
		{pad + inner*0.20, pad + inner},     // 左下的谷
		{pad + inner*0.5, pad + inner*0.30}, // 中间那个尖
		{pad + inner*0.80, pad + inner},     // 右下的谷
		{pad + inner, pad},
	}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			on := false
			for i := 0; i < len(pts)-1 && !on; i++ {
				if distToSegment(px, py, pts[i], pts[i+1]) <= width {
					on = true
				}
			}
			if !on {
				continue
			}
			i := ((s-1-y)*s + x) * 4
			color[i], color[i+1], color[i+2], color[i+3] = 0xFF, 0xFF, 0xFF, 0xFF
		}
	}
	return color, mask
}

// inRoundedRect 判断点是否落在圆角矩形内。
func inRoundedRect(px, py, x0, y0, w, h, r float64) bool {
	if px < x0 || py < y0 || px > x0+w || py > y0+h {
		return false
	}
	// 四个角：圆心在距边 r 处，落在圆外则剔除。
	cx, cy := x0+r, y0+r
	switch {
	case px < x0+r && py < y0+r:
	case px > x0+w-r && py < y0+r:
		cx = x0 + w - r
	case px < x0+r && py > y0+h-r:
		cy = y0 + h - r
	case px > x0+w-r && py > y0+h-r:
		cx, cy = x0+w-r, y0+h-r
	default:
		return true
	}
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

// distToSegment 点到线段的距离。
func distToSegment(px, py float64, a, b struct{ x, y float64 }) float64 {
	vx, vy := b.x-a.x, b.y-a.y
	wx, wy := px-a.x, py-a.y
	len2 := vx*vx + vy*vy
	if len2 == 0 {
		return math.Hypot(wx, wy)
	}
	t := (wx*vx + wy*vy) / len2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(px-(a.x+t*vx), py-(a.y+t*vy))
}

// ---------------------------------------------------------------- 小工具

// utf16Of 把字符串转成 UTF-16 码元切片（用于定长的 WCHAR 数组）。
func utf16Of(s string) []uint16 {
	if s == "" {
		return nil
	}
	p, err := syscall.UTF16FromString(s)
	if err != nil {
		return nil
	}
	return p
}

// clipText 按**码元数**截断，避免半个代理对被写进定长 WCHAR 数组。
// Win32 的 szTip/szInfo 有硬上限，超了 Shell_NotifyIcon 直接失败（不截断）。
func clipText(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes])
}
