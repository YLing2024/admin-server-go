package sites

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
)

// collectBackups 列出备份目录的一级子目录，并用一次 `du -sb` 批量求和。
// 目录可能很多，逐个 fork du 会把页面拖慢，需求书明确禁止。
func (s *Service) collectBackups(ctx context.Context) BackupsPayload {
	payload := BackupsPayload{
		Backups: []Backup{},
		Timers:  []BackupTimer{},
	}
	var errs []string

	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		readErr := err
		s.log.Warn("备份目录读取失败", "dir", s.backupDir, "err", readErr.Error())
		payload.Timers, err = s.readBackupTimers(ctx)
		if err != nil {
			errs = append(errs, err.Error())
		}
		errs = append([]string{"读取备份目录失败: " + readErr.Error()}, errs...)
		payload.Error = strings.Join(errs, "; ")
		return payload
	}

	type dirInfo struct {
		name    string
		path    string
		modTime int64
	}
	dirs := make([]dirInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, dirInfo{
			name:    entry.Name(),
			path:    s.backupDir + string(os.PathSeparator) + entry.Name(),
			modTime: info.ModTime().UnixMilli(),
		})
	}

	if len(dirs) > 0 {
		args := make([]string, 0, len(dirs)+2)
		args = append(args, "-sb", "--")
		for _, dir := range dirs {
			args = append(args, dir.path)
		}
		out, duErr := s.run(ctx, "du", args...)
		sizes := parseDuSizes(out)
		if duErr != nil {
			s.log.Warn("du 统计失败", "err", duErr.Error())
			if len(sizes) == 0 {
				errs = append(errs, duErr.Error())
			}
		}
		for _, dir := range dirs {
			size := sizes[dir.path]
			payload.Backups = append(payload.Backups, Backup{
				Name:       dir.name,
				Size:       size,
				ModifiedAt: dir.modTime,
			})
			payload.TotalSize += size
			if dir.modTime > payload.LatestModifiedAt {
				payload.LatestModifiedAt = dir.modTime
			}
		}
		payload.Total = len(payload.Backups)
		sort.SliceStable(payload.Backups, func(i, j int) bool {
			return payload.Backups[i].ModifiedAt > payload.Backups[j].ModifiedAt
		})
		if len(payload.Backups) > backupLimit {
			payload.Backups = payload.Backups[:backupLimit]
		}
	}

	timers, timerErr := s.readBackupTimers(ctx)
	if timerErr != nil {
		errs = append(errs, timerErr.Error())
	}
	payload.Timers = timers
	if len(errs) > 0 {
		payload.Error = strings.Join(errs, "; ")
	}
	return payload
}

// parseDuSizes 解析 `du -sb` 的 `字节数<TAB>路径` 输出。
func parseDuSizes(out string) map[string]int64 {
	sizes := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 2)
		if len(fields) != 2 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		if err != nil {
			continue
		}
		sizes[fields[1]] = size
	}
	return sizes
}

// readBackupTimers 先列出全部 timer，再一次性 `systemctl show` 取两个时间属性。
// 只用一次 list 和一次 show：timer 数量不多，但也没必要每个单元 fork 一次。
func (s *Service) readBackupTimers(ctx context.Context) ([]BackupTimer, error) {
	out, err := s.run(ctx, "systemctl", "list-units", "--type=timer", "--all",
		"--no-legend", "--plain", "--no-pager")
	if err != nil {
		return []BackupTimer{}, err
	}

	names := make([]string, 0, 4)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if strings.Contains(strings.ToLower(name), "backup") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return []BackupTimer{}, nil
	}

	args := append([]string{"show", "--no-pager",
		"-p", "Id,NextElapseUSecRealtime,LastTriggerUSec"}, names...)
	out, err = s.run(ctx, "systemctl", args...)
	if err != nil {
		return []BackupTimer{}, err
	}
	timers := parseShowTimers(out)
	sort.SliceStable(timers, func(i, j int) bool { return timers[i].Name < timers[j].Name })
	return timers, nil
}

// parseShowTimers 解析 `systemctl show` 的 key=value 块（单元之间以空行分隔）。
func parseShowTimers(out string) []BackupTimer {
	timers := make([]BackupTimer, 0, 4)
	current := map[string]string{}
	flush := func() {
		if id := current["Id"]; id != "" {
			timers = append(timers, BackupTimer{
				Name:                   id,
				NextElapseUSecRealtime: current["NextElapseUSecRealtime"],
				LastTriggerUSec:        current["LastTriggerUSec"],
			})
		}
		current = map[string]string{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			current[key] = value
		}
	}
	flush()
	return timers
}
