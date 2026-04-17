package luamod

import (
	"crypto/md5" //nolint:gosec
	"encoding/hex"
	"math/rand"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// RegisterFP registers the `fp` global table into L.
// It provides fingerprint generation helpers that mirror the logic in
// internal/captcha/direct.go (generateSliderCursor, generateDeviceInfo, etc.).
func RegisterFP(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "mouse_path", L.NewFunction(fpMousePath))
	L.SetField(tbl, "device_info", L.NewFunction(fpDeviceInfo))
	L.SetField(tbl, "browser_fp", L.NewFunction(fpBrowserFP))
	L.SetField(tbl, "connection_rtt", L.NewFunction(fpConnectionRTT))
	L.SetField(tbl, "connection_downlink", L.NewFunction(fpConnectionDownlink))
	L.SetField(tbl, "random_adfp", L.NewFunction(fpRandomAdFP))
	L.SetGlobal("fp", tbl)
}

// newRand creates a *rand.Rand from an optional Lua integer seed (arg at index).
// If the arg is LNil or missing, a time-based seed is used.
func newRand(L *lua.LState, idx int) *rand.Rand {
	lv := L.Get(idx)
	if lv == lua.LNil || lv == nil {
		return rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	}
	seed := int64(L.CheckInt(idx))
	return rand.New(rand.NewSource(seed)) //nolint:gosec
}

// fpMousePath implements fp.mouse_path(x1, y1, x2, y2, num_points) → table of {x, y}.
// Ports generateSliderCursor from direct.go, but parameterised by start/end coords
// and requested point count.  x1/y1 are the starting area, x2/y2 the target, and
// num_points is the minimum path length hint (may produce more).
func fpMousePath(L *lua.LState) int {
	x1 := L.CheckInt(1)
	y1 := L.CheckInt(2)
	x2 := L.CheckInt(3)
	_ = L.CheckInt(4) // y2 — used as drag endpoint hint; y is free to vary
	numPoints := L.CheckInt(5)

	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec

	result := L.NewTable()

	// Start near the given origin area.
	x := x1 + r.Intn(20)
	y := y1 + r.Intn(20)
	addPoint := func(px, py int) {
		pt := L.NewTable()
		L.SetField(pt, "x", lua.LNumber(px))
		L.SetField(pt, "y", lua.LNumber(py))
		result.Append(pt)
	}
	addPoint(x, y)

	// Move towards the target handle.
	targetX := x2 + r.Intn(40)
	targetY := y1 + r.Intn(20)
	steps := 5 + r.Intn(5)
	for i := 1; i <= steps; i++ {
		px := x + (targetX-x)*i/steps + r.Intn(6) - 3
		py := y + (targetY-y)*i/steps + r.Intn(6) - 3
		addPoint(px, py)
	}

	// Drag right until we have at least numPoints.
	cx, cy := targetX, targetY
	for result.Len() < numPoints+2 {
		cx += 5 + r.Intn(15)
		cy += r.Intn(4) - 2
		addPoint(cx, cy)
	}

	// Final hold positions.
	for i := 0; i < 2+r.Intn(3); i++ {
		addPoint(cx+r.Intn(2), cy+r.Intn(2))
	}

	L.Push(result)
	return 1
}

// fpDeviceInfo implements fp.device_info(seed) → table.
// Ports generateDeviceInfo from direct.go.
func fpDeviceInfo(L *lua.LState) int {
	r := newRand(L, 1)

	widths := []int{1920, 2560, 1680, 1440}
	heights := []int{1080, 1440, 1050, 900}
	idx := r.Intn(len(widths))
	w, h := widths[idx], heights[idx]

	cores := []int{8, 12, 16, 24}
	mem := []int{8, 16, 32}

	tbl := L.NewTable()
	L.SetField(tbl, "screenWidth", lua.LNumber(w))
	L.SetField(tbl, "screenHeight", lua.LNumber(h))
	L.SetField(tbl, "screenAvailWidth", lua.LNumber(w))
	L.SetField(tbl, "screenAvailHeight", lua.LNumber(h-48))
	L.SetField(tbl, "innerWidth", lua.LNumber(w/2+r.Intn(200)))
	L.SetField(tbl, "innerHeight", lua.LNumber(h-100-r.Intn(100)))
	L.SetField(tbl, "devicePixelRatio", lua.LNumber(1))
	L.SetField(tbl, "language", lua.LString("ru"))

	langs := L.NewTable()
	langs.Append(lua.LString("ru"))
	L.SetField(tbl, "languages", langs)

	L.SetField(tbl, "webdriver", lua.LFalse)
	L.SetField(tbl, "hardwareConcurrency", lua.LNumber(cores[r.Intn(len(cores))]))
	L.SetField(tbl, "deviceMemory", lua.LNumber(mem[r.Intn(len(mem))]))
	L.SetField(tbl, "connectionEffectiveType", lua.LString("4g"))
	L.SetField(tbl, "notificationsPermission", lua.LString("denied"))

	L.Push(tbl)
	return 1
}

// fpBrowserFP implements fp.browser_fp(seed) → 32-char hex string.
// Ports generateBrowserFp: MD5 of 32 random bytes.
func fpBrowserFP(L *lua.LState) int {
	r := newRand(L, 1)
	data := make([]byte, 32)
	r.Read(data) //nolint:gosec
	h := md5.Sum(data) //nolint:gosec
	L.Push(lua.LString(hex.EncodeToString(h[:])))
	return 1
}

// fpConnectionRTT implements fp.connection_rtt(count) → table of ints.
// Ports generateConnectionRtt: count values in range [50, 150].
func fpConnectionRTT(L *lua.LState) int {
	count := L.CheckInt(1)
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	base := 50 + r.Intn(100)
	tbl := L.NewTable()
	for i := 0; i < count; i++ {
		tbl.Append(lua.LNumber(base))
	}
	L.Push(tbl)
	return 1
}

// fpConnectionDownlink implements fp.connection_downlink(count) → table of floats.
// Ports generateConnectionDownlink: count values in range [5.0, 20.0].
func fpConnectionDownlink(L *lua.LState) int {
	count := L.CheckInt(1)
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	base := 5.0 + float64(r.Intn(15))
	tbl := L.NewTable()
	for i := 0; i < count; i++ {
		tbl.Append(lua.LNumber(base))
	}
	L.Push(tbl)
	return 1
}

// fpRandomAdFP implements fp.random_adfp() → 21-char string.
// Ports randomAdFp from direct.go.
func fpRandomAdFP(L *lua.LState) int {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	b := make([]byte, 21)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	L.Push(lua.LString(b))
	return 1
}
