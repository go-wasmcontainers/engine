package lineedit

import gw "github.com/go-widgets/lineedit"

// The types, as aliases: values pass between the two paths unchanged.
type (
	Candidate = gw.Candidate
	Completer = gw.Completer
	Editor    = gw.Editor
	Event     = gw.Event
	Key       = gw.Key
	KeyCode   = gw.KeyCode
	Options   = gw.Options
	Partial   = gw.Partial
	Reader    = gw.Reader
	Renderer  = gw.Renderer
	State     = gw.State
)

// MaxLine bounds a line, as in go-widgets/lineedit.
const MaxLine = gw.MaxLine

// The key codes.
const (
	KeyRune      = gw.KeyRune
	KeyEnter     = gw.KeyEnter
	KeyBackspace = gw.KeyBackspace
	KeyDelete    = gw.KeyDelete
	KeyLeft      = gw.KeyLeft
	KeyRight     = gw.KeyRight
	KeyHome      = gw.KeyHome
	KeyEnd       = gw.KeyEnd
	KeyUp        = gw.KeyUp
	KeyDown      = gw.KeyDown
	KeyTab       = gw.KeyTab
	KeyEscape    = gw.KeyEscape
	KeyCtrlA     = gw.KeyCtrlA
	KeyCtrlB     = gw.KeyCtrlB
	KeyCtrlC     = gw.KeyCtrlC
	KeyCtrlD     = gw.KeyCtrlD
	KeyCtrlE     = gw.KeyCtrlE
	KeyCtrlF     = gw.KeyCtrlF
	KeyCtrlG     = gw.KeyCtrlG
	KeyCtrlK     = gw.KeyCtrlK
	KeyCtrlL     = gw.KeyCtrlL
	KeyCtrlN     = gw.KeyCtrlN
	KeyCtrlP     = gw.KeyCtrlP
	KeyCtrlR     = gw.KeyCtrlR
	KeyCtrlU     = gw.KeyCtrlU
	KeyCtrlW     = gw.KeyCtrlW
	KeyUnknown   = gw.KeyUnknown
)

// The errors are the same values, so errors.Is holds across the two paths.
var (
	ErrInterrupt    = gw.ErrInterrupt
	ErrUnterminated = gw.ErrUnterminated
)

// The functions.
var (
	Format       = gw.Format
	New          = gw.New
	ParseKeys    = gw.ParseKeys
	Quote        = gw.Quote
	R            = gw.R
	RuneWidth    = gw.RuneWidth
	Split        = gw.Split
	SplitPartial = gw.SplitPartial
	Width        = gw.Width
)
