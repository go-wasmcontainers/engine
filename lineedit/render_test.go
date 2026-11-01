package lineedit

import (
	"strconv"
	"strings"
	"testing"
)

// vt is a tiny terminal emulator with xterm's wrapping: printing in the
// last column leaves the cursor there with a pending wrap, the next
// printable character wraps first.
type vt struct {
	cols        int
	rows        [][]rune
	r, c        int
	wrapPending bool
}

func (v *vt) row(i int) []rune {
	for len(v.rows) <= i {
		v.rows = append(v.rows, make([]rune, v.cols))
	}
	return v.rows[i]
}

func (v *vt) write(s string) {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		switch {
		case ch == '\r':
			v.c, v.wrapPending = 0, false
		case ch == '\n':
			v.r++
			v.wrapPending = false
			v.row(v.r)
		case ch == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == ';') {
				j++
			}
			n, _ := strconv.Atoi(strings.Split(string(rs[i+2:j]), ";")[0])
			if n == 0 {
				n = 1
			}
			switch rs[j] {
			case 'A':
				v.r -= n
			case 'B':
				v.r += n
			case 'C':
				v.c += n
			case 'D':
				v.c -= n
			case 'J':
				row := v.row(v.r)
				for k := v.c; k < v.cols; k++ {
					row[k] = 0
				}
				v.rows = v.rows[:v.r+1]
			case 'K':
				row := v.row(v.r)
				for k := v.c; k < v.cols; k++ {
					row[k] = 0
				}
			}
			v.wrapPending = false
			i = j
		default:
			if v.wrapPending {
				v.r++
				v.c = 0
				v.wrapPending = false
			}
			v.row(v.r)[v.c] = ch
			if v.c == v.cols-1 {
				v.wrapPending = true
			} else {
				v.c++
			}
		}
	}
}

func (v *vt) text() string {
	var lines []string
	for _, r := range v.rows {
		lines = append(lines, strings.TrimRight(strings.Map(func(c rune) rune {
			if c == 0 {
				return ' '
			}
			return c
		}, string(r)), " "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
}

func TestRender(t *testing.T) {
	for _, cols := range []int{10, 12, 80} {
		v := &vt{cols: cols}
		r := &Renderer{Cols: cols, PromptStyle: "-"}
		e := New(Options{})
		show := func() { v.write(r.Render(e.State())) }
		show()
		for _, ch := range "wctr run hello" {
			e.Key(nil, R(ch))
			show()
		}
		want := "$ wctr run hello"
		flat := strings.ReplaceAll(v.text(), "\n", "")
		if flat != want {
			t.Fatalf("cols %d: %q", cols, v.text())
		}
		// The cursor is where the editor says, after moving and deleting.
		for i := 0; i < 5; i++ { // to just after the blank before "hello"
			e.Key(nil, Key{Code: KeyLeft})
		}
		e.Key(nil, Key{Code: KeyBackspace})
		show()
		if flat := strings.ReplaceAll(v.text(), "\n", ""); flat != "$ wctr runhello" {
			t.Fatalf("cols %d: %q", cols, v.text())
		}
		pos := 2 + e.State().Cursor
		if v.r != pos/cols || v.c != pos%cols {
			t.Fatalf("cols %d: cursor at %d,%d, want %d,%d", cols, v.r, v.c, pos/cols, pos%cols)
		}
		// Printing a listing keeps the line below it.
		v.write(r.Print([]string{"one", "two"}, e.State()))
		if !strings.Contains(v.text(), "one\ntwo\n") || !strings.HasSuffix(strings.ReplaceAll(v.text(), "\n", ""), "$ wctr runhello") {
			t.Fatalf("cols %d: %q", cols, v.text())
		}
		v.write(r.Leave(e.State()))
		if r.Leave(e.State()) != "" {
			t.Fatal("leave twice")
		}
	}
	// A line that exactly fills a row.
	v := &vt{cols: 10}
	r := &Renderer{Cols: 10}
	v.write(r.Render(State{Prompt: "$ ", Line: "12345678", Cursor: 8}))
	if v.r != 1 || v.c != 0 {
		t.Fatalf("cursor %d,%d", v.r, v.c)
	}
	v.write(r.Render(State{Prompt: "$ ", Line: "123456789", Cursor: 9}))
	if strings.ReplaceAll(v.text(), "\n", "") != "$ 123456789" {
		t.Fatalf("%q", v.text())
	}
	r.Forget()
	if (&Renderer{}).cols() != 80 {
		t.Fatal("default cols")
	}
	if s := (&Renderer{Cols: 80, PromptStyle: "\x1b[1m"}).Render(State{Prompt: "> "}); !strings.HasPrefix(s, "\x1b[1m> \x1b[0m") {
		t.Fatalf("%q", s)
	}
}
