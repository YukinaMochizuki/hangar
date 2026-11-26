//go:build testbed

// Command memhog holds memory in a guest, and times it, for the memory
// pools' tests (test/testbed/vms/pool_test.go).
//
//	memhog MiB   allocates and touches MiB of anonymous memory a MiB at a
//	             time, says "held MiB" once it holds it all, and holds it,
//	             touching it again now and then so it stays in use
//	memhog tick  wakes every 10 ms and says, every second, the most a wake
//	             came late in that second: "late N ms"
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: memhog MiB | memhog tick")
		os.Exit(2)
	}
	if os.Args[1] == "tick" {
		tick()
	}
	mib, err := strconv.Atoi(os.Args[1])
	if err != nil || mib < 0 {
		fmt.Fprintln(os.Stderr, "usage: memhog MiB | memhog tick")
		os.Exit(2)
	}
	held := make([][]byte, 0, mib)
	for range mib {
		b := make([]byte, 1<<20)
		for j := 0; j < len(b); j += 4096 {
			b[j] = 1
		}
		held = append(held, b)
	}
	fmt.Printf("held %d\n", len(held))
	for {
		time.Sleep(10 * time.Second)
		for _, b := range held {
			b[0]++
		}
	}
}

// tick says how late its wakes come: a vCPU the host throttles, or stops,
// wakes late.
func tick() {
	const every = 10 * time.Millisecond
	last, window := time.Now(), time.Now()
	var worst time.Duration
	for {
		time.Sleep(every)
		now := time.Now()
		worst = max(worst, now.Sub(last)-every)
		last = now
		if now.Sub(window) >= time.Second {
			fmt.Printf("late %d ms\n", worst.Milliseconds())
			worst, window = 0, now
		}
	}
}
