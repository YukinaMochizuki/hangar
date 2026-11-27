//go:build testbed

// Command memhog holds memory in a guest, and times it, for the memory
// pools' tests (test/testbed/vms/pool_test.go).
//
//	memhog MiB   allocates and touches MiB of anonymous memory a MiB at a
//	             time, says "held MiB" once it holds it all, and holds it,
//	             touching it again now and then so it stays in use
//	memhog tick  wakes every 10 ms and says, every second, the most a wake
//	             came late in that second: "late N ms"
//	memhog burn N  keeps N threads busy, for the CPU pools' tests
//	             (test/testbed/vms/cpupool_test.go), and says "burning N"
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "burn" {
		burn(os.Args[2])
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: memhog MiB | memhog tick | memhog burn N")
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

// burn keeps n threads busy, each a vCPU's worth while it may run, and
// never returns.
func burn(arg string) {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		fmt.Fprintln(os.Stderr, "usage: memhog burn N")
		os.Exit(2)
	}
	for range n {
		go func() {
			for x := 0; ; x++ {
				_ = x
			}
		}()
	}
	fmt.Printf("burning %d\n", n)
	select {}
}
