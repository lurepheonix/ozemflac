package main

import (
	"fmt"
	"runtime/debug"
)

// version is the release tag (e.g. "v0.1.0"). Override at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" .
var version = "dev"

// versionString reports the binary version. Precedence: ldflags release
// tag, then VCS info stamped by the Go toolchain, then "dev".
func versionString() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		rev, t := "", ""
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				t = s.Value
			}
		}
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}
		switch {
		case short != "" && t != "":
			return "dev (" + short + ", " + t + ")"
		case short != "":
			return "dev (" + short + ")"
		}
	}
	return "dev"
}

func printVersion() {
	fmt.Println("ozemflac " + versionString())
}
