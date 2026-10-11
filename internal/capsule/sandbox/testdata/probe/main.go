// probe is a hostile test capsule: each command tries to break one isolation
// guarantee and prints "OK" if it succeeded or "DENIED: <err>" if it was stopped.
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	arg := ""
	if len(os.Args) > 2 {
		arg = os.Args[2]
	}
	n, _ := strconv.Atoi(arg)
	var err error
	switch os.Args[1] {
	case "read":
		_, err = os.ReadFile(arg)
	case "ls":
		_, err = os.ReadDir(arg)
	case "write":
		err = os.WriteFile(arg, []byte("x"), 0o644)
	case "exec":
		err = exec.Command(arg).Run()
	case "tcp":
		var c net.Conn
		if c, err = net.DialTimeout("tcp", arg, 2*time.Second); err == nil {
			c.Close()
		}
	case "unix":
		var c net.Conn
		if c, err = net.Dial("unix", arg); err == nil {
			c.Close()
		}
	case "unshare":
		err = syscall.Unshare(syscall.CLONE_NEWUSER)
	case "mount":
		err = syscall.Mount("tmpfs", "/data", "tmpfs", 0, "")
	case "kill":
		err = syscall.Kill(n, syscall.SIGKILL)
	case "alloc": // MiB, touched so it counts as RSS
		b := make([]byte, n<<20)
		for i := range b {
			b[i] = 1
		}
		time.Sleep(2 * time.Second) // hold it long enough for the no-cgroup watchdog
		b[0]++
	case "fork":
		for i := 0; i < n && err == nil; i++ {
			err = exec.Command("/proc/self/exe", "sleep").Start()
		}
	case "files":
		for i := 0; i < n && err == nil; i++ {
			_, err = os.Open("/dev/null")
		}
	case "orphan": // leave a child running, then exit
		err = exec.Command("/proc/self/exe", "sleep").Start()
	case "sleep":
		time.Sleep(time.Hour)
	case "exit":
		os.Exit(n)
	case "procs": // number of processes it can see
		ents, _ := os.ReadDir("/proc")
		pids := 0
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				pids++
			}
		}
		fmt.Println(pids)
		return
	case "env":
		fmt.Println(os.Environ())
		return
	}
	if err != nil {
		fmt.Println("DENIED:", err)
	} else {
		fmt.Println("OK")
	}
}
