// Command greenline is a merge queue with a content-addressed build cache.
//
//	greenline build  [-root dir] [-remote url | -cache dir] [targets...]
//	greenline cache  [-addr :9092] [-dir dir] [-max-bytes n]
//	greenline sim    [flags]                       (see sim.go)
//	greenline serve  [flags]                       (see serve.go)
//	greenline demo   [flags]                       (see demo.go)
package main

import (
	"fmt"
	"log/slog"
	"os"
)

var commands = map[string]func(args []string) error{}

func register(name string, f func([]string) error) { commands[name] = f }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	f, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := f(os.Args[2:]); err != nil {
		slog.Error(os.Args[1], "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: greenline <command> [flags]\n\ncommands:")
	for _, c := range []string{"build", "cache", "sim", "bench", "serve", "demo"} {
		if _, ok := commands[c]; ok {
			fmt.Fprintln(os.Stderr, "  "+c)
		}
	}
}

func logger(verbose bool) *slog.Logger {
	lvl := slog.LevelInfo
	if verbose {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
