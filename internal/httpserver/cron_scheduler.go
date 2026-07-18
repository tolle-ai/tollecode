package httpserver

// cron_scheduler.go — fires cron-type todo tasks on schedule.
//
// On StartAPI the scheduler ticks once per minute aligned to the wall clock.
// For every loaded workspace it scans tasks with ScheduleType=="cron", parses
// the 5-field cron expression, and calls runTodoTask when the expression matches
// the current minute.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/tolle-ai/tollecode/internal/todo"
)

// startCronScheduler runs the cron engine in the background.
// It exits cleanly when ctx is cancelled.
func startCronScheduler(ctx context.Context, state *apiState) {
	go func() {
		// Align to the start of the next whole minute so the first tick fires at
		// HH:MM:00 and subsequent ticks stay aligned.
		now := time.Now()
		delay := time.Duration(60-now.Second())*time.Second -
			time.Duration(now.Nanosecond())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				fireCronTasks(ctx, state, t)
			}
		}
	}()
}

// fireCronTasks scans all workspace stores and fires tasks whose cron expression
// matches now.  A task that is already running is skipped.
func fireCronTasks(ctx context.Context, state *apiState, now time.Time) {
	for _, t := range allCronTasks() {
		if t.Cron == "" || !cronMatches(t.Cron, now) {
			continue
		}
		if t.Status == "running" {
			continue
		}
		// Reset step statuses so the task runs fresh.
		for i := range t.Steps {
			t.Steps[i].Status = "pending"
			t.Steps[i].SessionID = ""
		}
		t.Status = "pending"
		todo.Update(t.WorkspacePath, t)
		go runTodoTask(state, t.ID, t.WorkspacePath)
	}
}

// allCronTasks returns copies of every cron task across all loaded workspace stores.
func allCronTasks() []*todo.Task {
	var out []*todo.Task
	// Iterate every registered workspace.
	globalRegistry.mu.RLock()
	paths := make([]string, 0, len(globalRegistry.localWorkspaces))
	for _, w := range globalRegistry.localWorkspaces {
		paths = append(paths, w.Path)
	}
	globalRegistry.mu.RUnlock()

	for _, p := range paths {
		for _, t := range todo.List(p) {
			if t.ScheduleType == "cron" {
				cp := *t
				out = append(out, &cp)
			}
		}
	}
	return out
}

// ── Cron expression parser ───────────────────────────────────────────────────
//
// Supports standard 5-field cron: minute hour dom month dow
// Each field may be: * | */n | a-b | a,b,c | a number

func cronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return matchField(fields[0], t.Minute(), 0, 59) &&
		matchField(fields[1], t.Hour(), 0, 23) &&
		matchField(fields[2], t.Day(), 1, 31) &&
		matchField(fields[3], int(t.Month()), 1, 12) &&
		matchField(fields[4], int(t.Weekday()), 0, 6)
}

func matchField(field string, val, _, _ int) bool {
	if field == "*" {
		return true
	}
	// Step: */n or start/n
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		step, err := strconv.Atoi(parts[1])
		if err != nil || step <= 0 {
			return false
		}
		if parts[0] == "*" {
			return val%step == 0
		}
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return false
		}
		return val >= start && (val-start)%step == 0
	}
	// List: check each element, which may itself be a range
	for _, item := range strings.Split(field, ",") {
		if matchSingle(item, val) {
			return true
		}
	}
	return false
}

func matchSingle(item string, val int) bool {
	if strings.Contains(item, "-") {
		parts := strings.SplitN(item, "-", 2)
		lo, err1 := strconv.Atoi(parts[0])
		hi, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return false
		}
		return val >= lo && val <= hi
	}
	n, err := strconv.Atoi(item)
	return err == nil && n == val
}
