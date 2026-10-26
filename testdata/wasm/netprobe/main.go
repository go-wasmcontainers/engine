// Command netprobe is the networking test guest: a stock Go wasip1 program
// using github.com/stealthrocket/net (the WasmEdge socket extension), as a
// user's image would.
//
//	netprobe serve PORT          HTTP server: GET /x answers "hello x"
//	netprobe get URL [N]         N GETs (default 1), bodies on stdout
//	netprobe udp-echo PORT       echoes datagrams, forever
//	netprobe udp HOST:PORT MSG   sends MSG, prints the reply
//	netprobe lookup NAME         prints NAME's addresses
//	netprobe dial HOST:PORT      dials, prints the error (or "connected")
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/stealthrocket/net/wasip1"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: netprobe serve|get|udp-echo|udp|lookup|dial ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		l, err := wasip1.Listen("tcp", ":"+os.Args[2])
		check(err)
		fmt.Println("listening on", l.Addr())
		check(http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello %s from %s\n", r.URL.Path[1:], r.RemoteAddr)
		})))
	case "get":
		n := 1
		if len(os.Args) > 3 {
			n, _ = strconv.Atoi(os.Args[3])
		}
		c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: wasip1.DialContext}}
		for i := 0; i < n; i++ {
			resp, err := c.Get(os.Args[2])
			check(err)
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			check(err)
			fmt.Print(string(b))
		}
	case "udp-echo":
		pc, err := wasip1.ListenPacket("udp", ":"+os.Args[2])
		check(err)
		fmt.Println("echoing on", pc.LocalAddr())
		buf := make([]byte, 2048)
		for {
			k, from, err := pc.ReadFrom(buf)
			check(err)
			pc.WriteTo(buf[:k], from)
		}
	case "udp":
		c, err := wasip1.Dial("udp", os.Args[2])
		check(err)
		_, err = c.Write([]byte(os.Args[3]))
		check(err)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 2048)
		k, err := c.Read(buf)
		check(err)
		fmt.Printf("reply: %s\n", buf[:k])
	case "lookup":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupHost(ctx, os.Args[2])
		check(err)
		fmt.Println(ips)
	case "dial":
		c, err := wasip1.Dial("tcp", os.Args[2])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("connected")
		c.Close()
	default:
		fmt.Fprintln(os.Stderr, "netprobe: unknown command", os.Args[1])
		os.Exit(2)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "netprobe:", err)
		os.Exit(1)
	}
}
