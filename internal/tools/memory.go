package tools

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"sync"
)

// Code gets as much memory as the machine can spare when it starts, within these. Its
// /tmp and /dev/shm are held in memory and charged to it, so writing files there leaves
// less for everything else, and they can't fill up first.
const (
	codeMemoryMin = 2 << 30
	codeMemoryMax = 96 << 30
	// codeMemoryMargin is the least left for tack, the system and everything else. A tenth
	// of the machine is left if that's more.
	codeMemoryMargin = 8 << 30
)

// budgets keeps track of the memory each running sandbox was given, so that two
// starting together don't each count the same free memory.
type budgets struct {
	mu      sync.Mutex
	running map[*int64]bool
}

// meminfo reads what the kernel says is in use. MemAvailable already leaves out what it
// could take back, like the page cache.
func meminfo() (total, available int64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	found := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := bytes.Fields(scanner.Bytes())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(string(fields[1]), 10, 64)
		if err != nil {
			continue
		}
		switch string(fields[0]) {
		case "MemTotal:":
			total, found = kb<<10, found+1
		case "MemAvailable:":
			available, found = kb<<10, found+1
		}
	}
	if found != 2 {
		return 0, 0, errors.New("/proc/meminfo doesn't say how much memory there is")
	}
	return total, available, nil
}

// memoryLimit works out a starting sandbox's memory from what's available, less a margin
// and less what's already promised to other sandboxes. release gives it back.
func (b *budgets) memoryLimit(total, available int64) (limit int64, release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spare := available - max(codeMemoryMargin, total/10)
	for promised := range b.running {
		spare -= *promised
	}
	// Whole pages, so /tmp's size is exactly the limit.
	limit = min(max(spare, codeMemoryMin), codeMemoryMax) &^ (4<<10 - 1)
	if b.running == nil {
		b.running = map[*int64]bool{}
	}
	key := new(int64)
	*key = limit
	b.running[key] = true
	return limit, func() {
		b.mu.Lock()
		delete(b.running, key)
		b.mu.Unlock()
	}
}
