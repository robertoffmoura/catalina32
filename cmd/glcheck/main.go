// Command glcheck stands in for the viewer on the OpenGL stream of a Source game run
// (C32_GL_SHM): it checks every command, answers questions, and prints statistics at the end
// (on SIGINT/SIGTERM, or when the game ends).
package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"catalina32/pkg/source"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: glcheck <gl.shm>")
		os.Exit(2)
	}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	k, err := source.RunGLCheck(os.Args[1], stop)
	if err != nil {
		fmt.Println("glcheck:", err)
	}
	fmt.Printf("glcheck: %d commands, %.1f MB, %d frames, %d questions, %d errors\n",
		k.Commands, float64(k.Bytes)/(1<<20), k.Frames, k.Questions, len(k.Errors))
	for _, e := range k.Errors {
		fmt.Println("  error:", e)
	}
	var names []string
	for n := range k.PerCall {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return k.PerCall[names[i]] > k.PerCall[names[j]] })
	for i, n := range names {
		if i == 25 {
			break
		}
		fmt.Printf("  %9d %s\n", k.PerCall[n], n)
	}
	if err != nil || len(k.Errors) > 0 {
		os.Exit(1)
	}
}
