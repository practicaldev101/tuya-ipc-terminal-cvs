package main

import (
	"fmt"
	"os"

	"tuya-ipc-terminal/cmd"
	"tuya-ipc-terminal/pkg/core"
)

// VERSION is a var (not a const) so release builds can stamp it with
// -ldflags "-X main.VERSION=...".
var VERSION = "0.0.6-cvs.1"

func main() {
	core.InitLogger()

	if err := cmd.Execute(VERSION); err != nil {
		fmt.Println("Command execution failed")
		os.Exit(1)
	}
}
