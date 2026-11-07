package monitor

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type processRecord struct {
	start         uint64
	groups        map[string]string
	owner         int
	hasReferences bool
}

type procSnapshot struct {
	records            map[uint32]processRecord
	roots              map[int]processRecord
	references         map[int]map[string]int
	sharedReferences   map[int]int
	unresolved         int
	complete           bool
	referencesComplete bool
}

type fdReferences struct {
	physical   map[string]bool
	shared     bool
	unresolved int
	complete   bool
}

func readBounded(name string, max int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("collection_limit")
	}
	return b, nil
}

func readProcess(root string, pid uint32) (processRecord, error) {
	base := filepath.Join(root, strconv.FormatUint(uint64(pid), 10))
	first, err := readStart(base)
	if err != nil {
		return processRecord{}, err
	}
	data, err := readBounded(filepath.Join(base, "cgroup"), 65536)
	if err != nil {
		return processRecord{}, err
	}
	groups := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "/") {
			return processRecord{}, errors.New("invalid_cgroup")
		}
		key := parts[0] + ":" + parts[1]
		if _, exists := groups[key]; exists {
			return processRecord{}, errors.New("ambiguous_cgroup")
		}
		groups[key] = path.Clean(parts[2])
	}
	if len(groups) == 0 {
		return processRecord{}, errors.New("missing_cgroup")
	}
	last, err := readStart(base)
	if err != nil || first != last {
		return processRecord{}, errors.New("process_changed")
	}
	return processRecord{start: first, groups: groups, owner: -1}, nil
}

func readStart(base string) (uint64, error) {
	data, err := readBounded(filepath.Join(base, "stat"), 16384)
	if err != nil {
		return 0, err
	}
	// comm is parenthesized and may itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, errors.New("invalid_stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return 0, errors.New("invalid_stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, errors.New("invalid_starttime")
	}
	return start, nil
}

var mountPathDecoder = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// PID 1 can live in init.scope while services occupy sibling cgroups. The
// container's cgroup mount root, not PID 1's leaf group, bounds ownership.
func readInstanceRoot(root string, pid uint32) (processRecord, error) {
	record, err := readProcess(root, pid)
	if err != nil {
		return processRecord{}, err
	}
	data, err := readBounded(filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "mountinfo"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil // Retain the narrower observed group when no mount is available.
	}
	if err != nil {
		return processRecord{}, err
	}
	after, err := readProcess(root, pid)
	if err != nil || after.start != record.start || !sameGroups(after.groups, record.groups) {
		return processRecord{}, errors.New("process_changed")
	}
	groups := after.groups
	for _, line := range strings.Split(string(data), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, options := strings.Fields(left), strings.Fields(right)
		if len(fields) < 6 || len(options) < 3 {
			continue
		}
		mountRoot := mountPathDecoder.Replace(fields[3])
		mountpoint := mountPathDecoder.Replace(fields[4])
		if mountRoot == "/" || !strings.HasPrefix(mountRoot, "/") || path.Clean(mountRoot) != mountRoot {
			continue // Never broaden ownership to the host's hierarchy root.
		}
		for controller, group := range record.groups {
			_, names, _ := strings.Cut(controller, ":")
			matches := options[0] == "cgroup2" && controller == "0:" && mountpoint == "/sys/fs/cgroup"
			if options[0] == "cgroup" && names != "" && strings.HasPrefix(mountpoint, "/sys/fs/cgroup/") {
				matches = true
				for _, name := range strings.Split(names, ",") {
					if !strings.Contains(","+options[2]+",", ","+name+",") {
						matches = false
					}
				}
			}
			if matches && (group == mountRoot || strings.HasPrefix(group, mountRoot+"/")) {
				groups[controller] = mountRoot
			}
		}
	}
	record.groups = groups
	return record, nil
}

func belongs(groups, root map[string]string) bool {
	matched := false
	for controller, base := range root {
		if base == "/" {
			continue
		}
		group, ok := groups[controller]
		if !ok || (group != base && !strings.HasPrefix(group, base+"/")) {
			return false
		}
		matched = true
	}
	return matched
}

func boundedEntries(ctx context.Context, name string, max int) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(max + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > max {
		return entries[:max], errors.New("collection_limit")
	}
	return entries, nil
}

func (e *Engine) scanProcesses(ctx context.Context, instances []Instance) procSnapshot {
	result := procSnapshot{records: make(map[uint32]processRecord), roots: make(map[int]processRecord), references: make(map[int]map[string]int), sharedReferences: make(map[int]int), complete: true, referencesComplete: true}
	for i, instance := range instances {
		if i >= e.maxProcesses {
			result.complete = false
			break
		}
		if ctx.Err() != nil {
			result.complete = false
			break
		}
		if instance.Type != "container" || instance.InitPID <= 0 {
			continue
		}
		root, err := readInstanceRoot(e.procRoot, uint32(instance.InitPID))
		if err != nil {
			result.complete = false
			continue
		}
		valid := false
		for _, group := range root.groups {
			if group != "/" {
				valid = true
			}
		}
		if !valid {
			result.complete = false
			continue
		}
		result.roots[i] = root
	}
	// Index exact roots, then walk ancestors. Avoid comparing every process to
	// every instance; slash-separated ancestors also enforce the 1 vs 10 boundary.
	index := make(map[string]map[string][]int)
	for owner, root := range result.roots {
		for controller, group := range root.groups {
			if group == "/" {
				continue
			}
			if index[controller] == nil {
				index[controller] = make(map[string][]int)
			}
			index[controller][group] = append(index[controller][group], owner)
		}
	}
	entries, err := boundedEntries(ctx, e.procRoot, e.maxProcesses)
	if err != nil {
		result.complete = false
	}
	remaining := e.maxFDs
	for _, entry := range entries {
		if ctx.Err() != nil {
			result.complete = false
			break
		}
		pid, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil || pid == 0 {
			continue
		}
		rec, err := readProcess(e.procRoot, uint32(pid))
		if err != nil {
			result.complete = false
			continue
		}
		candidates := 0
	ownership:
		for controller, group := range rec.groups {
			for depth := 0; group != "/"; depth++ {
				if depth >= 256 || ctx.Err() != nil {
					rec.owner = -2
					result.complete = false
					break ownership
				}
				for _, owner := range index[controller][group] {
					candidates++
					if candidates > 4096 {
						rec.owner = -2
						result.complete = false
						break ownership
					}
					if owner == rec.owner || !belongs(rec.groups, result.roots[owner].groups) {
						continue
					}
					if rec.owner != -1 {
						rec.owner = -2
						result.complete = false
						break ownership
					}
					rec.owner = owner
				}
				group = path.Dir(group)
			}
		}
		result.records[uint32(pid)] = rec
		if rec.owner < 0 {
			continue
		}
		refs := e.scanFDs(ctx, uint32(pid), &remaining)
		after, err := readProcess(e.procRoot, uint32(pid))
		if err != nil || after.start != rec.start || !sameGroups(after.groups, rec.groups) {
			result.complete = false
			continue
		}
		rec.hasReferences = len(refs.physical) > 0 || refs.shared || refs.unresolved > 0
		result.records[uint32(pid)] = rec
		if !refs.complete {
			result.referencesComplete = false
		}
		result.unresolved += refs.unresolved
		if refs.shared {
			result.sharedReferences[rec.owner]++
		}
		if len(refs.physical) > 0 {
			if result.references[rec.owner] == nil {
				result.references[rec.owner] = make(map[string]int)
			}
			for uuid := range refs.physical {
				result.references[rec.owner][uuid]++
			}
		}
	}
	return result
}

func sameGroups(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func (e *Engine) scanFDs(ctx context.Context, pid uint32, remaining *int) fdReferences {
	refs := fdReferences{}
	if *remaining <= 0 {
		return refs
	}
	base := filepath.Join(e.procRoot, strconv.FormatUint(uint64(pid), 10), "fd")
	entries, err := boundedEntries(ctx, base, *remaining)
	refs.complete = err == nil
	for _, entry := range entries {
		if ctx.Err() != nil {
			refs.complete = false
			return refs
		}
		*remaining -= 1
		name := filepath.Join(base, entry.Name())
		info, err := os.Stat(name)
		if err != nil {
			refs.complete = false
			continue
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			continue
		}
		rdev, ok := deviceNumber(info)
		if !ok {
			refs.complete = false
			continue
		}
		if e.controlDevices[rdev] {
			refs.shared = true
			continue
		}
		if uuid, exists := e.devices[rdev]; exists {
			if uuid == "" {
				refs.unresolved++
				refs.complete = false
			} else {
				if refs.physical == nil {
					refs.physical = make(map[string]bool)
				}
				refs.physical[uuid] = true
			}
			continue
		}
		target, err := os.Readlink(name)
		if err != nil {
			refs.complete = false
			continue
		}
		leaf := filepath.Base(target)
		if strings.HasPrefix(leaf, "nvidia") || strings.Contains(target, "/dri/") {
			refs.unresolved++
			refs.complete = false
		}
	}
	return refs
}
