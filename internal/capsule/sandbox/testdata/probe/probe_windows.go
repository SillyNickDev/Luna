package main

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func osProbe(cmd, arg string, n int) error {
	switch cmd {
	case "kill":
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(n))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		return windows.TerminateProcess(h, 1)
	case "registry": // read a value under HKCU\Software\<arg>
		k, err := registry.OpenKey(registry.CURRENT_USER, `Software\`+arg, registry.QUERY_VALUE)
		if err != nil {
			return err
		}
		defer k.Close()
		_, _, err = k.GetStringValue("secret")
		return err
	}
	return errors.New("unknown probe " + cmd)
}
