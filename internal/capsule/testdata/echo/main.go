// echo is a test capsule speaking the control protocol. argv[1] picks its behaviour:
// ok (provides "echo"), consumer, nohello, nopong, exit.
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"luna/internal/capsule"
)

func main() {
	mode := os.Args[1]
	if mode == "nohello" {
		time.Sleep(time.Hour)
	}
	c, err := net.Dial("unix", os.Getenv("CAPSULE_CONTROL"))
	if err != nil {
		fmt.Println("dial control:", err)
		os.Exit(1)
	}
	ctl := capsule.NewConn(c)
	ctl.Write("hello", 0, capsule.Hello{Spec: "0.1", PID: os.Getpid()})
	if f, err := ctl.Read(); err != nil || f.T != "welcome" {
		os.Exit(1)
	}
	ctl.Write("log", 0, capsule.Log{Level: "info", Msg: "hello from " + mode})
	ctl.Write("ready", 0, nil)
	if mode == "exit" {
		time.Sleep(200 * time.Millisecond)
		os.Exit(3)
	}
	for {
		f, err := ctl.Read()
		if err != nil {
			os.Exit(0)
		}
		switch f.T {
		case "ping":
			if mode != "nopong" {
				ctl.Write("pong", f.ID, nil)
			}
		case "shutdown":
			fmt.Println("shutting down")
			os.Exit(0)
		case "call":
			var lc capsule.LinkCall
			capsule.Decode(f.B, &lc)
			ctl.Write("result", f.ID, capsule.Result{OK: true, Body: mustCBOR(lc.Link + "/" + lc.Method + " from " + lc.Caller)})
		case "link_up":
			var l capsule.LinkName
			capsule.Decode(f.B, &l)
			go consume(l.Name)
		}
	}
}

func consume(use string) {
	lc, err := net.Dial("unix", filepath.Join(os.Getenv("CAPSULE_LINKS"), use))
	if err != nil {
		fmt.Println("dial link:", err)
		return
	}
	conn := capsule.NewConn(lc)
	conn.Write("call", 1, capsule.Call{Method: "say", Caller: "dev.evil.spoof"})
	f, err := conn.Read()
	var r capsule.Result
	var s string
	if err == nil && capsule.Decode(f.B, &r) == nil && capsule.Decode(r.Body, &s) == nil {
		fmt.Println("result:", r.OK, s)
	} else {
		fmt.Println("call failed:", err, r.Error)
	}
	// An oversized frame must close the connection.
	lc.Write([]byte{0x00, 0x20, 0x00, 0x00}) // claims 2 MiB
	lc.Write(make([]byte, 4096))
	if _, err := conn.Read(); err != nil {
		fmt.Println("big frame refused")
	}
}

func mustCBOR(s string) []byte {
	var buf []byte
	buf = append(buf, 0x78, byte(len(s))) // text string, 1-byte length
	return append(buf, s...)
}
