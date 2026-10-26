package main

import (
	"os"
	"testing"
)

func TestMainExitStatus(t *testing.T) {
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"wctr", "-H", "unix:///nonexistent/x.sock", "ps"}
	main()
	if code != 1 {
		t.Fatal(code)
	}
}
