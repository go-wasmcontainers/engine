// probe exercises what a container may do, one subcommand at a time, so the
// engine's tests can check the guest's view of its world.
//
//	probe exit N        writes "bye" to stderr and exits N
//	probe args ...      prints each argument on its own line
//	probe env           prints the environment, one variable per line
//	probe cwd           prints the working directory
//	probe cat PATH      prints a file
//	probe ls PATH       lists a directory
//	probe fs            writes, renames, reads back and removes files in /tmp
//	probe spin          loops for ever (wall-clock cancel)
//	probe hog           allocates until memory runs out
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <cmd> ...")
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "exit":
		code := 3
		if len(args) > 0 {
			code, _ = strconv.Atoi(args[0])
		}
		fmt.Fprintln(os.Stderr, "bye")
		os.Exit(code)
	case "args":
		for _, a := range args {
			fmt.Println(a)
		}
	case "env":
		env := os.Environ()
		sort.Strings(env)
		for _, e := range env {
			fmt.Println(e)
		}
	case "cwd":
		wd, err := os.Getwd()
		check(err)
		fmt.Println(wd)
	case "cat":
		b, err := os.ReadFile(args[0])
		check(err)
		os.Stdout.Write(b)
	case "ls":
		ents, err := os.ReadDir(args[0])
		check(err)
		for _, e := range ents {
			fmt.Println(e.Name())
		}
	case "fs":
		fsProbe()
	case "spin":
		for i := 0; ; i++ {
			if i < 0 {
				fmt.Println(i)
			}
		}
	case "hog":
		var keep [][]byte
		for {
			b := make([]byte, 1<<20)
			for i := range b {
				b[i] = byte(i)
			}
			keep = append(keep, b)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func fsProbe() {
	check(os.MkdirAll("/tmp/probe/sub", 0o755))
	check(os.WriteFile("/tmp/probe/a.txt", []byte("alpha"), 0o644))
	check(os.Rename("/tmp/probe/a.txt", "/tmp/probe/sub/b.txt"))
	b, err := os.ReadFile("/tmp/probe/sub/b.txt")
	check(err)
	fmt.Println("read:", string(b))
	f, err := os.OpenFile("/tmp/probe/sub/b.txt", os.O_APPEND|os.O_WRONLY, 0)
	check(err)
	_, err = f.WriteString("+beta")
	check(err)
	check(f.Close())
	b, err = os.ReadFile("/tmp/probe/sub/b.txt")
	check(err)
	fmt.Println("appended:", string(b))
	check(os.Symlink("sub/b.txt", "/tmp/probe/link"))
	t, err := os.Readlink("/tmp/probe/link")
	check(err)
	fmt.Println("link:", t)
	b, err = os.ReadFile("/tmp/probe/link")
	check(err)
	fmt.Println("via link:", string(b))
	st, err := os.Stat("/tmp/probe/sub/b.txt")
	check(err)
	fmt.Println("size:", st.Size())
	check(os.Remove("/tmp/probe/link"))
	check(os.Remove("/tmp/probe/sub/b.txt"))
	check(os.Remove("/tmp/probe/sub"))
	ents, err := os.ReadDir("/tmp/probe")
	check(err)
	fmt.Println("left:", len(ents))
	// Writing over an image file must stay inside this container.
	check(os.WriteFile("/etc/motd", []byte("changed\n"), 0o644))
	b, err = os.ReadFile("/etc/motd")
	check(err)
	fmt.Print("motd: ", string(b))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}
